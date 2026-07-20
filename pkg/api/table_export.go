package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana/pkg/api/response"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/tsdb"
	"github.com/grafana/grafana/pkg/tsdb/sqleng"
)

const (
	maxInt64Value            = int64(1<<63 - 1)
	excelMaxDataRowsPerSheet = 1048575
	excelMaxCellRunes        = 32767
	excelColumnWidthSamples  = 500
	excelBodyCellStyle       = 0
	excelHeaderCellStyle     = 1
	excelOneDecimalCellStyle = 2
	excelWorksheetBufferSize = 256 * 1024
)

type tableExportColumn struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Hidden      bool   `json:"hidden"`
}

type tableExportRequest struct {
	From    string               `json:"from"`
	To      string               `json:"to"`
	Queries []*simplejson.Json   `json:"queries"`
	Debug   bool                 `json:"debug"`
	Title   string               `json:"title"`
	Columns []tableExportColumn  `json:"columns"`
}

type tableExportData struct {
	name      string
	columns   []string
	rowCount  int
	cellValue func(row, column int) interface{}
}

type tableExportSheet struct {
	name     string
	data     tableExportData
	rowStart int
	rowEnd   int
}

type tableExportResponse struct {
	filename string
	sheets   []tableExportSheet
}

type tableCountResponse struct {
	TotalRows int64 `json:"totalRows"`
}

func (r *tableExportResponse) Body() []byte {
	return nil
}

func (r *tableExportResponse) Status() int {
	return http.StatusOK
}

func (r *tableExportResponse) WriteTo(ctx *models.ReqContext) {
	header := ctx.Resp.Header()
	header.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": r.filename}))
	header.Set("Cache-Control", "no-store")
	ctx.Resp.WriteHeader(http.StatusOK)

	if err := writeTableExportWorkbook(ctx.Resp, r.sheets); err != nil {
		ctx.Logger.Error("Failed to stream Excel table export", "error", err)
	}
}

// QueryTableCount runs lightweight count wrappers for SQL table queries so a capped dashboard preview can show the full total.
func (hs *HTTPServer) QueryTableCount(c *models.ReqContext, reqDTO tableExportRequest) response.Response {
	if len(reqDTO.Queries) == 0 {
		return response.Error(http.StatusBadRequest, "No queries found in count request", nil)
	}

	request := &tsdb.TsdbQuery{
		TimeRange: tsdb.NewTimeRange(reqDTO.From, reqDTO.To),
		Debug:     reqDTO.Debug,
		User:      c.SignedInUser,
		Queries:   make([]*tsdb.Query, 0, len(reqDTO.Queries)),
	}

	var ds *models.DataSource
	for i, query := range reqDTO.Queries {
		rawSQL := query.Get("rawSql").MustString()
		countSQL, err := buildTableCountSQL(rawSQL)
		if err != nil {
			return response.Error(http.StatusBadRequest, "Unable to count table rows", err)
		}
		query.Set("rawSql", countSQL)
		query.Set("format", "table")
		query.Set(sqleng.FullTableExportQueryFlag, true)

		datasourceID, err := query.Get("datasourceId").Int64()
		if err != nil {
			return response.Error(http.StatusBadRequest, "Count query missing data source ID", nil)
		}
		if i == 0 {
			ds, err = hs.DatasourceCache.GetDatasource(datasourceID, c.SignedInUser, c.SkipCache)
			if err != nil {
				return hs.handleGetDataSourceError(err, datasourceID)
			}
		}

		request.Queries = append(request.Queries, &tsdb.Query{
			RefId:         query.Get("refId").MustString("A"),
			MaxDataPoints: 1,
			IntervalMs:    query.Get("intervalMs").MustInt64(1000),
			QueryType:     query.Get("queryType").MustString(""),
			Model:         query,
			DataSource:    ds,
		})
	}

	if err := hs.PluginRequestValidator.Validate(ds.Url, nil); err != nil {
		return response.Error(http.StatusForbidden, "Access denied", err)
	}

	queryResponse, err := tsdb.HandleRequest(c.Req.Context(), ds, request)
	if err != nil {
		return response.Error(http.StatusInternalServerError, "Table row count failed", err)
	}
	for _, result := range queryResponse.Results {
		if result.Error != nil {
			return response.Error(http.StatusBadRequest, "Table row count failed", result.Error)
		}
	}

	countData, err := collectTableExportData(queryResponse, nil)
	if err != nil {
		return response.Error(http.StatusInternalServerError, "Unable to read table row count", err)
	}
	var totalRows int64
	for _, data := range countData {
		if data.rowCount == 0 || len(data.columns) == 0 {
			continue
		}
		count, ok := tableCountValue(data.cellValue(0, 0))
		if !ok {
			return response.Error(http.StatusInternalServerError, "Table row count returned an invalid value", nil)
		}
		totalRows += count
	}

	return response.JSON(http.StatusOK, tableCountResponse{TotalRows: totalRows})
}

func buildTableCountSQL(rawSQL string) (string, error) {
	rawSQL = strings.TrimRight(strings.TrimSpace(rawSQL), "; \t\r\n")
	if rawSQL == "" {
		return "", fmt.Errorf("table query is empty")
	}

	// Most table panels use a simple SELECT ... FROM ... WHERE ... ORDER BY query.
	// Replacing the projection directly avoids materializing wide rows and removes
	// an unnecessary sort, which is substantially faster for multi-million-row
	// job history tables. Queries whose row cardinality can be changed by DISTINCT,
	// GROUP BY, aggregates, CTEs, or set operators keep the safe subquery fallback.
	tokens := scanTopLevelSQLTokens(rawSQL)
	countSource := stripTopLevelSQLTail(rawSQL, tokens)
	if fromStart, ok := simpleCountFromStart(tokens, len(countSource)); ok {
		return "SELECT COUNT(*) AS total_rows " + strings.TrimSpace(countSource[fromStart:]), nil
	}

	return fmt.Sprintf("SELECT COUNT(*) AS total_rows FROM (%s) AS grafana_table_count", countSource), nil
}

type topLevelSQLToken struct {
	word  string
	start int
	end   int
}

// scanTopLevelSQLTokens returns words outside parentheses, quoted values, and comments.
// This is intentionally a small cardinality parser rather than a dialect-specific SQL parser.
func scanTopLevelSQLTokens(query string) []topLevelSQLToken {
	var tokens []topLevelSQLToken
	depth := 0
	for index := 0; index < len(query); {
		switch query[index] {
		case '\'', '"', '`':
			quote := query[index]
			index++
			for index < len(query) {
				if query[index] == '\\' {
					index += 2
					continue
				}
				if query[index] == quote {
					if index+1 < len(query) && query[index+1] == quote {
						index += 2
						continue
					}
					index++
					break
				}
				index++
			}
			continue
		case '[':
			index++
			for index < len(query) {
				if query[index] == ']' {
					if index+1 < len(query) && query[index+1] == ']' {
						index += 2
						continue
					}
					index++
					break
				}
				index++
			}
			continue
		case '-':
			if index+1 < len(query) && query[index+1] == '-' {
				index += 2
				for index < len(query) && query[index] != '\n' {
					index++
				}
				continue
			}
		case '#':
			index++
			for index < len(query) && query[index] != '\n' {
				index++
			}
			continue
		case '/':
			if index+1 < len(query) && query[index+1] == '*' {
				index += 2
				for index+1 < len(query) && !(query[index] == '*' && query[index+1] == '/') {
					index++
				}
				if index+1 < len(query) {
					index += 2
				}
				continue
			}
		case '(':
			depth++
			index++
			continue
		case ')':
			if depth > 0 {
				depth--
			}
			index++
			continue
		}

		if isSQLWordStart(query[index]) {
			start := index
			index++
			for index < len(query) && isSQLWordPart(query[index]) {
				index++
			}
			if depth == 0 {
				tokens = append(tokens, topLevelSQLToken{
					word:  strings.ToUpper(query[start:index]),
					start: start,
					end:   index,
				})
			}
			continue
		}
		index++
	}
	return tokens
}

func isSQLWordStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isSQLWordPart(value byte) bool {
	return isSQLWordStart(value) || value >= '0' && value <= '9' || value == '$'
}

// stripTopLevelSQLTail removes clauses that do not affect the uncapped row total.
func stripTopLevelSQLTail(query string, tokens []topLevelSQLToken) string {
	for index, token := range tokens {
		switch token.word {
		case "ORDER":
			if index+1 < len(tokens) && tokens[index+1].word == "BY" {
				return strings.TrimSpace(query[:token.start])
			}
		case "LIMIT", "OFFSET", "FETCH":
			return strings.TrimSpace(query[:token.start])
		}
	}
	return strings.TrimSpace(query)
}

func simpleCountFromStart(tokens []topLevelSQLToken, queryEnd int) (int, bool) {
	if len(tokens) == 0 || tokens[0].word != "SELECT" {
		return 0, false
	}

	fromIndex := -1
	for index := 1; index < len(tokens); index++ {
		if tokens[index].start >= queryEnd {
			break
		}
		if tokens[index].word == "FROM" {
			fromIndex = index
			break
		}
	}
	if fromIndex < 0 {
		return 0, false
	}

	for index := 1; index < fromIndex; index++ {
		switch tokens[index].word {
		case "DISTINCT", "TOP", "INTO", "COUNT", "SUM", "AVG", "MIN", "MAX", "GROUP_CONCAT", "STRING_AGG", "ARRAY_AGG", "JSON_AGG":
			return 0, false
		}
	}

	for index := fromIndex + 1; index < len(tokens) && tokens[index].start < queryEnd; index++ {
		switch tokens[index].word {
		case "GROUP", "HAVING", "UNION", "INTERSECT", "EXCEPT", "QUALIFY":
			return 0, false
		}
	}

	return tokens[fromIndex].start, true
}

func tableCountValue(value interface{}) (int64, bool) {
	value = dereferenceTableExportValue(value)
	if value == nil {
		return 0, false
	}

	switch typed := value.(type) {
	case json.Number:
		count, err := typed.Int64()
		return count, err == nil && count >= 0
	case []byte:
		count, err := strconv.ParseInt(strings.TrimSpace(string(typed)), 10, 64)
		return count, err == nil && count >= 0
	case string:
		count, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return count, err == nil && count >= 0
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		count := reflected.Int()
		return count, count >= 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		count := reflected.Uint()
		if count > uint64(maxInt64Value) {
			return 0, false
		}
		return int64(count), true
	case reflect.Float32, reflect.Float64:
		count := reflected.Float()
		if math.IsNaN(count) || math.IsInf(count, 0) || count < 0 || count > float64(maxInt64Value) || math.Trunc(count) != count {
			return 0, false
		}
		return int64(count), true
	default:
		return 0, false
	}
}

// QueryTableExcel reruns a SQL table query on the server and streams the complete result as XLSX.
// Rows beyond Excel's per-sheet limit are automatically split across numbered sheets.
func (hs *HTTPServer) QueryTableExcel(c *models.ReqContext, reqDTO tableExportRequest) response.Response {
	if len(reqDTO.Queries) == 0 {
		return response.Error(http.StatusBadRequest, "No queries found in export request", nil)
	}

	request := &tsdb.TsdbQuery{
		TimeRange: tsdb.NewTimeRange(reqDTO.From, reqDTO.To),
		Debug:     reqDTO.Debug,
		User:      c.SignedInUser,
		Queries:   make([]*tsdb.Query, 0, len(reqDTO.Queries)),
	}

	var ds *models.DataSource
	for i, query := range reqDTO.Queries {
		query.Set(sqleng.FullTableExportQueryFlag, true)
		datasourceID, err := query.Get("datasourceId").Int64()
		if err != nil {
			return response.Error(http.StatusBadRequest, "Export query missing data source ID", nil)
		}

		if i == 0 {
			ds, err = hs.DatasourceCache.GetDatasource(datasourceID, c.SignedInUser, c.SkipCache)
			if err != nil {
				return hs.handleGetDataSourceError(err, datasourceID)
			}
		}

		request.Queries = append(request.Queries, &tsdb.Query{
			RefId:         query.Get("refId").MustString("A"),
			MaxDataPoints: query.Get("maxDataPoints").MustInt64(100),
			IntervalMs:    query.Get("intervalMs").MustInt64(1000),
			QueryType:     query.Get("queryType").MustString(""),
			Model:         query,
			DataSource:    ds,
		})
	}

	if err := hs.PluginRequestValidator.Validate(ds.Url, nil); err != nil {
		return response.Error(http.StatusForbidden, "Access denied", err)
	}

	queryResponse, err := tsdb.HandleRequest(c.Req.Context(), ds, request)
	if err != nil {
		return response.Error(http.StatusInternalServerError, "Excel export query failed", err)
	}

	for _, result := range queryResponse.Results {
		if result.Error != nil {
			return response.Error(http.StatusBadRequest, "Excel export query failed", result.Error)
		}
	}

	data, err := collectTableExportData(queryResponse, reqDTO.Columns)
	if err != nil {
		return response.Error(http.StatusInternalServerError, "Unable to prepare Excel export", err)
	}
	if len(data) == 0 {
		return response.Error(http.StatusBadRequest, "The query returned no table data", nil)
	}

	title := strings.TrimSpace(reqDTO.Title)
	if title == "" {
		title = "grafana-table"
	}

	return &tableExportResponse{
		filename: fmt.Sprintf("%s-%s.xlsx", sanitizeExportFilename(title), time.Now().Format("2006-01-02_15-04-05")),
		sheets:   splitTableExportSheets(data, title, excelMaxDataRowsPerSheet),
	}
}

func collectTableExportData(queryResponse *tsdb.Response, requestedColumns []tableExportColumn) ([]tableExportData, error) {
	resultNames := make([]string, 0, len(queryResponse.Results))
	for name := range queryResponse.Results {
		resultNames = append(resultNames, name)
	}
	sort.Strings(resultNames)

	var exportData []tableExportData
	for _, resultName := range resultNames {
		result := queryResponse.Results[resultName]
		for tableIndex, table := range result.Tables {
			columnNames := make([]string, len(table.Columns))
			for index, column := range table.Columns {
				columnNames[index] = column.Text
			}
			if len(columnNames) == 0 {
				continue
			}
			indexes, names := selectExportColumns(columnNames, requestedColumns)
			capturedTable := table
			capturedIndexes := indexes
			exportData = append(exportData, tableExportData{
				name:     exportDataName(resultName, tableIndex),
				columns:  names,
				rowCount: len(capturedTable.Rows),
				cellValue: func(row, column int) interface{} {
					if row < 0 || row >= len(capturedTable.Rows) || column < 0 || column >= len(capturedIndexes) {
						return nil
					}
					sourceColumn := capturedIndexes[column]
					if sourceColumn < 0 || sourceColumn >= len(capturedTable.Rows[row]) {
						return nil
					}
					return capturedTable.Rows[row][sourceColumn]
				},
			})
		}

		if len(result.Tables) == 0 && result.Dataframes != nil {
			frames, decodeErr := result.Dataframes.Decoded()
			if decodeErr != nil {
				return nil, decodeErr
			}
			for frameIndex, frame := range frames {
				if len(frame.Fields) == 0 {
					continue
				}
				columnNames := make([]string, len(frame.Fields))
				rowCount := 0
				for index, field := range frame.Fields {
					columnNames[index] = field.Name
					if index == 0 || field.Len() < rowCount {
						rowCount = field.Len()
					}
				}
				indexes, names := selectExportColumns(columnNames, requestedColumns)
				capturedFrame := frame
				capturedIndexes := indexes
				exportData = append(exportData, tableExportData{
					name:     exportDataName(resultName, frameIndex),
					columns:  names,
					rowCount: rowCount,
					cellValue: func(row, column int) interface{} {
						if row < 0 || column < 0 || column >= len(capturedIndexes) {
							return nil
						}
						sourceColumn := capturedIndexes[column]
						if sourceColumn < 0 || sourceColumn >= len(capturedFrame.Fields) || row >= capturedFrame.Fields[sourceColumn].Len() {
							return nil
						}
						return capturedFrame.Fields[sourceColumn].At(row)
					},
				})
			}
		}
	}

	return exportData, nil
}

func selectExportColumns(sourceNames []string, requested []tableExportColumn) ([]int, []string) {
	if len(requested) > 0 {
		var indexes []int
		var names []string
		for _, column := range requested {
			if column.Hidden {
				continue
			}
			for sourceIndex, sourceName := range sourceNames {
				if sourceName == column.Name {
					indexes = append(indexes, sourceIndex)
					name := strings.TrimSpace(column.DisplayName)
					if name == "" {
						name = sourceName
					}
					names = append(names, name)
					break
				}
			}
		}
		if len(indexes) > 0 {
			return indexes, names
		}
	}

	indexes := make([]int, len(sourceNames))
	names := make([]string, len(sourceNames))
	for index, name := range sourceNames {
		indexes[index] = index
		names[index] = name
	}
	return indexes, names
}

func exportDataName(resultName string, index int) string {
	if index == 0 {
		return resultName
	}
	return fmt.Sprintf("%s_%d", resultName, index+1)
}

func splitTableExportSheets(data []tableExportData, title string, maxRows int) []tableExportSheet {
	if maxRows < 1 {
		maxRows = excelMaxDataRowsPerSheet
	}

	var sheets []tableExportSheet
	usedNames := make(map[string]bool)
	for dataIndex, item := range data {
		partCount := 1
		if item.rowCount > 0 {
			partCount = (item.rowCount + maxRows - 1) / maxRows
		}
		for part := 0; part < partCount; part++ {
			start := part * maxRows
			end := start + maxRows
			if end > item.rowCount {
				end = item.rowCount
			}

			baseName := title
			if len(data) > 1 {
				baseName = fmt.Sprintf("%s_%s", title, item.name)
			}
			if partCount > 1 {
				baseName = fmt.Sprintf("%s_%d", baseName, part+1)
			}
			if dataIndex > 0 && strings.TrimSpace(item.name) == "" {
				baseName = fmt.Sprintf("%s_%d", baseName, dataIndex+1)
			}

			sheets = append(sheets, tableExportSheet{
				name:     uniqueSheetName(baseName, usedNames),
				data:     item,
				rowStart: start,
				rowEnd:   end,
			})
		}
	}
	return sheets
}

func uniqueSheetName(value string, used map[string]bool) string {
	base := sanitizeSheetName(value)
	name := base
	for suffix := 2; used[strings.ToLower(name)]; suffix++ {
		suffixText := fmt.Sprintf("_%d", suffix)
		name = truncateRunes(base, 31-len([]rune(suffixText))) + suffixText
	}
	used[strings.ToLower(name)] = true
	return name
}

func sanitizeSheetName(value string) string {
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', '?', '*', '[', ']', ':':
			return '_'
		default:
			return r
		}
	}, strings.TrimSpace(value))
	if value == "" {
		value = "Table"
	}
	return truncateRunes(value, 31)
}

func sanitizeExportFilename(value string) string {
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		default:
			return r
		}
	}, strings.TrimSpace(value))
	if value == "" {
		value = "grafana-table"
	}
	return truncateRunes(value, 120)
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

func writeTableExportWorkbook(writer io.Writer, sheets []tableExportSheet) error {
	zipWriter := zip.NewWriter(writer)
	zipWriter.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})

	if err := writeWorkbookStaticFiles(zipWriter, sheets); err != nil {
		_ = zipWriter.Close()
		return err
	}
	for index, sheet := range sheets {
		entry, err := zipWriter.Create(fmt.Sprintf("xl/worksheets/sheet%d.xml", index+1))
		if err != nil {
			_ = zipWriter.Close()
			return err
		}
		bufferedEntry := bufio.NewWriterSize(entry, excelWorksheetBufferSize)
		if err := writeWorksheet(bufferedEntry, sheet); err != nil {
			_ = zipWriter.Close()
			return err
		}
		if err := bufferedEntry.Flush(); err != nil {
			_ = zipWriter.Close()
			return err
		}
	}

	return zipWriter.Close()
}

func writeWorkbookStaticFiles(zipWriter *zip.Writer, sheets []tableExportSheet) error {
	if err := writeZipText(zipWriter, "_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`); err != nil {
		return err
	}

	var contentTypes strings.Builder
	contentTypes.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for index := range sheets {
		fmt.Fprintf(&contentTypes, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, index+1)
	}
	contentTypes.WriteString(`</Types>`)
	if err := writeZipText(zipWriter, "[Content_Types].xml", contentTypes.String()); err != nil {
		return err
	}

	var workbook strings.Builder
	workbook.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for index, sheet := range sheets {
		workbook.WriteString(`<sheet name="`)
		if err := xml.EscapeText(&workbook, []byte(sheet.name)); err != nil {
			return err
		}
		fmt.Fprintf(&workbook, `" sheetId="%d" r:id="rId%d"/>`, index+1, index+1)
	}
	workbook.WriteString(`</sheets></workbook>`)
	if err := writeZipText(zipWriter, "xl/workbook.xml", workbook.String()); err != nil {
		return err
	}

	var relationships strings.Builder
	relationships.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for index := range sheets {
		fmt.Fprintf(&relationships, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, index+1, index+1)
	}
	fmt.Fprintf(&relationships, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`, len(sheets)+1)
	if err := writeZipText(zipWriter, "xl/_rels/workbook.xml.rels", relationships.String()); err != nil {
		return err
	}

	return writeZipText(zipWriter, "xl/styles.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><numFmts count="1"><numFmt numFmtId="164" formatCode="0.0"/></numFmts><fonts count="2"><font><sz val="12"/><color rgb="FF000000"/><name val="宋体"/></font><font><b/><sz val="12"/><color rgb="FF000000"/><name val="宋体"/></font></fonts><fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills><borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyFont="1" applyAlignment="1"><alignment horizontal="center" vertical="center"/></xf><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1" applyAlignment="1"><alignment horizontal="center" vertical="center"/></xf><xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyFont="1" applyNumberFormat="1" applyAlignment="1"><alignment horizontal="center" vertical="center"/></xf></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`)
}

func writeZipText(zipWriter *zip.Writer, name, value string) error {
	entry, err := zipWriter.Create(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(entry, value)
	return err
}

func writeWorksheet(writer io.Writer, sheet tableExportSheet) error {
	columnCount := len(sheet.data.columns)
	lastColumn := "A"
	if columnCount > 0 {
		lastColumn = excelColumnName(columnCount - 1)
	}
	lastRow := sheet.rowEnd - sheet.rowStart + 1
	if lastRow < 1 {
		lastRow = 1
	}

	if _, err := fmt.Fprintf(writer, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><dimension ref="A1:%s%d"/><sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews><cols>`, lastColumn, lastRow); err != nil {
		return err
	}
	for index, name := range sheet.data.columns {
		width := tableExportColumnWidth(sheet, index, name)
		if _, err := fmt.Fprintf(writer, `<col min="%d" max="%d" width="%d" customWidth="1"/>`, index+1, index+1, width); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(writer, `</cols><sheetData><row r="1">`); err != nil {
		return err
	}
	for column, name := range sheet.data.columns {
		if err := writeInlineStringCell(writer, excelColumnName(column)+"1", name, excelHeaderCellStyle); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(writer, `</row>`); err != nil {
		return err
	}

	excelColumns := make([]string, columnCount)
	columnStyles := make([]int, columnCount)
	for column, name := range sheet.data.columns {
		excelColumns[column] = excelColumnName(column)
		columnStyles[column] = tableExportCellStyle(name)
	}

	var rowBuffer bytes.Buffer
	rowBuffer.Grow(columnCount * 96)
	for sourceRow := sheet.rowStart; sourceRow < sheet.rowEnd; sourceRow++ {
		rowBuffer.Reset()
		excelRow := sourceRow - sheet.rowStart + 2
		excelRowText := strconv.Itoa(excelRow)
		if _, err := fmt.Fprintf(&rowBuffer, `<row r="%d">`, excelRow); err != nil {
			return err
		}
		for column := range sheet.data.columns {
			if err := writeTableExportCell(&rowBuffer, excelColumns[column], excelRowText, sheet.data.cellValue(sourceRow, column), columnStyles[column]); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(&rowBuffer, `</row>`); err != nil {
			return err
		}
		if _, err := writer.Write(rowBuffer.Bytes()); err != nil {
			return err
		}
	}

	if _, err := io.WriteString(writer, `</sheetData></worksheet>`); err != nil {
		return err
	}
	return nil
}

func tableExportCellStyle(columnName string) int {
	name := strings.ToLower(strings.Join(strings.Fields(columnName), ""))
	if strings.Contains(name, "指定内存") || strings.Contains(name, "requestedmemory") || strings.Contains(name, "specifiedmemory") {
		return excelOneDecimalCellStyle
	}
	return excelBodyCellStyle
}

func tableExportColumnWidth(sheet tableExportSheet, column int, name string) int {
	width := len([]rune(name))
	end := sheet.rowEnd
	if end > sheet.rowStart+excelColumnWidthSamples {
		end = sheet.rowStart + excelColumnWidthSamples
	}
	style := tableExportCellStyle(name)
	for row := sheet.rowStart; row < end; row++ {
		value := tableExportCellText(sheet.data.cellValue(row, column), style)
		if valueWidth := len([]rune(value)); valueWidth > width {
			width = valueWidth
		}
	}
	width += 2
	if width < 12 {
		return 12
	}
	if width > 60 {
		return 60
	}
	return width
}

func tableExportCellText(value interface{}, style int) string {
	value = dereferenceTableExportValue(value)
	if value == nil {
		return ""
	}
	if style == excelOneDecimalCellStyle {
		if formatted, ok := formatOneDecimalValue(value); ok {
			return formatted
		}
	}
	switch typed := value.(type) {
	case time.Time:
		return typed.Format(time.RFC3339Nano)
	case []byte:
		return string(typed)
	default:
		return fmt.Sprint(value)
	}
}

func dereferenceTableExportValue(value interface{}) interface{} {
	if value == nil {
		return nil
	}
	reflected := reflect.ValueOf(value)
	for reflected.IsValid() && (reflected.Kind() == reflect.Ptr || reflected.Kind() == reflect.Interface) {
		if reflected.IsNil() {
			return nil
		}
		reflected = reflected.Elem()
	}
	if !reflected.IsValid() {
		return nil
	}
	return reflected.Interface()
}

func formatOneDecimalValue(value interface{}) (string, bool) {
	var numericValue float64
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return "", false
		}
		numericValue = parsed
	case []byte:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(string(typed)), 64)
		if err != nil {
			return "", false
		}
		numericValue = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return "", false
		}
		numericValue = parsed
	default:
		reflected := reflect.ValueOf(value)
		switch reflected.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			numericValue = float64(reflected.Int())
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			numericValue = float64(reflected.Uint())
		case reflect.Float32, reflect.Float64:
			numericValue = reflected.Float()
		default:
			return "", false
		}
	}
	if math.IsNaN(numericValue) || math.IsInf(numericValue, 0) {
		return "", false
	}
	return strconv.FormatFloat(numericValue, 'f', 1, 64), true
}

func writeTableExportCell(writer io.Writer, columnReference, rowReference string, value interface{}, style int) error {
	if value == nil {
		return nil
	}

	value = dereferenceTableExportValue(value)
	if value == nil {
		return nil
	}
	reflected := reflect.ValueOf(value)

	if style == excelOneDecimalCellStyle {
		if formatted, ok := formatOneDecimalValue(value); ok {
			_, err := fmt.Fprintf(writer, `<c r="%s%s" s="%d"><v>%s</v></c>`, columnReference, rowReference, style, formatted)
			return err
		}
	}

	switch typed := value.(type) {
	case time.Time:
		return writeInlineStringCellParts(writer, columnReference, rowReference, typed.Format(time.RFC3339Nano), style)
	case json.Number:
		if _, err := fmt.Fprintf(writer, `<c r="%s%s" s="%d"><v>%s</v></c>`, columnReference, rowReference, style, typed.String()); err != nil {
			return err
		}
		return nil
	case []byte:
		return writeInlineStringCellParts(writer, columnReference, rowReference, string(typed), style)
	}

	switch reflected.Kind() {
	case reflect.Bool:
		value := 0
		if reflected.Bool() {
			value = 1
		}
		_, err := fmt.Fprintf(writer, `<c r="%s%s" s="%d" t="b"><v>%d</v></c>`, columnReference, rowReference, style, value)
		return err
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		_, err := fmt.Fprintf(writer, `<c r="%s%s" s="%d"><v>%d</v></c>`, columnReference, rowReference, style, reflected.Int())
		return err
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		_, err := fmt.Fprintf(writer, `<c r="%s%s" s="%d"><v>%d</v></c>`, columnReference, rowReference, style, reflected.Uint())
		return err
	case reflect.Float32, reflect.Float64:
		value := reflected.Float()
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return writeInlineStringCellParts(writer, columnReference, rowReference, fmt.Sprint(value), style)
		}
		_, err := fmt.Fprintf(writer, `<c r="%s%s" s="%d"><v>%s</v></c>`, columnReference, rowReference, style, strconv.FormatFloat(value, 'g', -1, 64))
		return err
	default:
		return writeInlineStringCellParts(writer, columnReference, rowReference, fmt.Sprint(value), style)
	}
}

func writeInlineStringCell(writer io.Writer, reference, value string, style int) error {
	return writeInlineStringCellParts(writer, reference, "", value, style)
}

func writeInlineStringCellParts(writer io.Writer, columnReference, rowReference, value string, style int) error {
	value = sanitizeXMLText(truncateRunes(value, excelMaxCellRunes))
	if _, err := fmt.Fprintf(writer, `<c r="%s%s" s="%d" t="inlineStr"><is><t xml:space="preserve">`, columnReference, rowReference, style); err != nil {
		return err
	}
	if err := xml.EscapeText(writer, []byte(value)); err != nil {
		return err
	}
	_, err := io.WriteString(writer, `</t></is></c>`)
	return err
}

func sanitizeXMLText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF) {
			return r
		}
		return -1
	}, value)
}

func excelColumnName(index int) string {
	name := ""
	for index >= 0 {
		name = string(rune('A'+index%26)) + name
		index = index/26 - 1
	}
	return name
}
