package api

import (
	"archive/zip"
	"bytes"
	"io/ioutil"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitTableExportSheets(t *testing.T) {
	data := tableExportData{
		name:     "A",
		columns:  []string{"id"},
		rowCount: 5,
		cellValue: func(row, column int) interface{} {
			return row
		},
	}

	sheets := splitTableExportSheets([]tableExportData{data}, "作业列表", 2)

	require.Len(t, sheets, 3)
	require.Equal(t, "作业列表_1", sheets[0].name)
	require.Equal(t, 0, sheets[0].rowStart)
	require.Equal(t, 2, sheets[0].rowEnd)
	require.Equal(t, "作业列表_3", sheets[2].name)
	require.Equal(t, 4, sheets[2].rowStart)
	require.Equal(t, 5, sheets[2].rowEnd)
}

func TestWriteTableExportWorkbook(t *testing.T) {
	rows := [][]interface{}{
		{"cnshalinhpc01", 12.5, true},
		{"cnshalinhpc02", nil, false},
	}
	data := tableExportData{
		name:     "A",
		columns:  []string{"主机名", "CPU利用率", "在线"},
		rowCount: len(rows),
		cellValue: func(row, column int) interface{} {
			return rows[row][column]
		},
	}
	sheets := splitTableExportSheets([]tableExportData{data}, "资源表", excelMaxDataRowsPerSheet)

	var output bytes.Buffer
	require.NoError(t, writeTableExportWorkbook(&output, sheets))

	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	require.NoError(t, err)
	files := make(map[string]string)
	for _, file := range reader.File {
		stream, openErr := file.Open()
		require.NoError(t, openErr)
		content, readErr := ioutil.ReadAll(stream)
		require.NoError(t, readErr)
		require.NoError(t, stream.Close())
		files[file.Name] = string(content)
	}

	require.Contains(t, files, "[Content_Types].xml")
	require.Contains(t, files, "xl/workbook.xml")
	require.Contains(t, files, "xl/styles.xml")
	require.Contains(t, files, "xl/worksheets/sheet1.xml")
	require.Contains(t, files["xl/styles.xml"], `<fonts count="2">`)
	require.Contains(t, files["xl/styles.xml"], `<b/>`)
	require.Contains(t, files["xl/styles.xml"], `<alignment horizontal="center" vertical="center"/>`)
	require.Contains(t, files["xl/workbook.xml"], `name="资源表"`)
	require.Contains(t, files["xl/worksheets/sheet1.xml"], "主机名")
	require.Contains(t, files["xl/worksheets/sheet1.xml"], "cnshalinhpc01")
	require.Contains(t, files["xl/worksheets/sheet1.xml"], `r="A1" s="1"`)
	require.Contains(t, files["xl/worksheets/sheet1.xml"], `r="A2" s="0"`)
	require.Contains(t, files["xl/worksheets/sheet1.xml"], `s="0"><v>12.5</v>`)
	require.Contains(t, files["xl/worksheets/sheet1.xml"], `s="0" t="b"><v>1</v>`)
}
