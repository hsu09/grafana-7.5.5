package api

import (
	"archive/zip"
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
)

const (
	excelMaxDataRowsPerSheet = 1048575
	excelMaxCellRunes        = 32767
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
		if err := writeWorksheet(entry, sheet); err != nil {
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
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts><fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills><borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`)
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
		width := len([]rune(name)) + 2
		if width < 12 {
			width = 12
		}
		if width > 60 {
			width = 60
		}
		if _, err := fmt.Fprintf(writer, `<col min="%d" max="%d" width="%d" customWidth="1"/>`, index+1, index+1, width); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(writer, `</cols><sheetData><row r="1">`); err != nil {
		return err
	}
	for column, name := range sheet.data.columns {
		if err := writeInlineStringCell(writer, excelColumnName(column)+"1", name); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(writer, `</row>`); err != nil {
		return err
	}

	for sourceRow := sheet.rowStart; sourceRow < sheet.rowEnd; sourceRow++ {
		excelRow := sourceRow - sheet.rowStart + 2
		if _, err := fmt.Fprintf(writer, `<row r="%d">`, excelRow); err != nil {
			return err
		}
		for column := range sheet.data.columns {
			if err := writeTableExportCell(writer, excelColumnName(column)+strconv.Itoa(excelRow), sheet.data.cellValue(sourceRow, column)); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(writer, `</row>`); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(writer, `</sheetData><autoFilter ref="A1:%s%d"/></worksheet>`, lastColumn, lastRow); err != nil {
		return err
	}
	return nil
}

func writeTableExportCell(writer io.Writer, reference string, value interface{}) error {
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
	value = reflected.Interface()

	switch typed := value.(type) {
	case time.Time:
		return writeInlineStringCell(writer, reference, typed.Format(time.RFC3339Nano))
	case json.Number:
		if _, err := fmt.Fprintf(writer, `<c r="%s"><v>%s</v></c>`, reference, typed.String()); err != nil {
			return err
		}
		return nil
	case []byte:
		return writeInlineStringCell(writer, reference, string(typed))
	}

	switch reflected.Kind() {
	case reflect.Bool:
		value := 0
		if reflected.Bool() {
			value = 1
		}
		_, err := fmt.Fprintf(writer, `<c r="%s" t="b"><v>%d</v></c>`, reference, value)
		return err
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		_, err := fmt.Fprintf(writer, `<c r="%s"><v>%d</v></c>`, reference, reflected.Int())
		return err
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		_, err := fmt.Fprintf(writer, `<c r="%s"><v>%d</v></c>`, reference, reflected.Uint())
		return err
	case reflect.Float32, reflect.Float64:
		value := reflected.Float()
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return writeInlineStringCell(writer, reference, fmt.Sprint(value))
		}
		_, err := fmt.Fprintf(writer, `<c r="%s"><v>%s</v></c>`, reference, strconv.FormatFloat(value, 'g', -1, 64))
		return err
	default:
		return writeInlineStringCell(writer, reference, fmt.Sprint(value))
	}
}

func writeInlineStringCell(writer io.Writer, reference, value string) error {
	value = sanitizeXMLText(truncateRunes(value, excelMaxCellRunes))
	if _, err := fmt.Fprintf(writer, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">`, reference); err != nil {
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
