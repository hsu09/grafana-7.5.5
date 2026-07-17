import { DataFrame, Field, formattedValueToString, getFieldDisplayName } from '@grafana/data';
import { TableFieldOptions, TableSortByFieldState } from '@grafana/ui/src/components/Table/types';
import * as XLSX from 'xlsx';

const DEFAULT_FILE_NAME = 'grafana-table';
const DEFAULT_SHEET_NAME = 'Table';
const MAX_DATA_ROWS_PER_SHEET = 1048575;
const SHEET_BUILD_CHUNK_SIZE = 10000;
const COLUMN_WIDTH_SAMPLE_ROWS = 500;

export type ExcelExportProgress = (completedRows: number, totalRows: number) => void;

function getVisibleFields(frame: DataFrame): Field[] {
  return frame.fields.filter((field) => {
    const options = (field.config.custom || {}) as TableFieldOptions;
    return !options.hidden;
  });
}

function compareValues(left: unknown, right: unknown): number {
  if (left === right) {
    return 0;
  }

  if (left === null || left === undefined) {
    return 1;
  }

  if (right === null || right === undefined) {
    return -1;
  }

  if (typeof left === 'number' && typeof right === 'number') {
    return left - right;
  }

  return String(left).localeCompare(String(right), undefined, {
    numeric: true,
    sensitivity: 'base',
  });
}

function getSortedRowIndexes(frame: DataFrame, fields: Field[], sortBy: TableSortByFieldState[]): number[] | undefined {
  if (!sortBy.length) {
    return undefined;
  }

  const indexes = Array.from({ length: frame.length }, (_, index) => index);

  const sortFields = sortBy
    .map((sort) => ({
      field: fields.find((field) => getFieldDisplayName(field, frame) === sort.displayName),
      desc: Boolean(sort.desc),
    }))
    .filter((sort): sort is { field: Field; desc: boolean } => Boolean(sort.field));

  if (!sortFields.length) {
    return undefined;
  }

  return indexes.sort((leftIndex, rightIndex) => {
    for (const sort of sortFields) {
      const result = compareValues(sort.field.values.get(leftIndex), sort.field.values.get(rightIndex));
      if (result !== 0) {
        return sort.desc ? -result : result;
      }
    }

    return leftIndex - rightIndex;
  });
}

function getCellValue(field: Field, rowIndex: number): string | number | boolean {
  const value = field.values.get(rowIndex);

  if (value === null || value === undefined) {
    return '';
  }

  if (field.display) {
    return formattedValueToString(field.display(value));
  }

  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
    return value;
  }

  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

function sanitizeFileName(title: string): string {
  const sanitized = title.replace(/[\\/:*?"<>|]+/g, '_').trim();
  return sanitized || DEFAULT_FILE_NAME;
}

function sanitizeSheetName(title: string, part: number, partCount: number): string {
  const sanitized = title.replace(/[\\/?*:[\]]+/g, '_').trim();
  const suffix = partCount > 1 ? `_${part + 1}` : '';
  return `${(sanitized || DEFAULT_SHEET_NAME).slice(0, 31 - suffix.length)}${suffix}`;
}

function getTimestamp(): string {
  return new Date().toISOString().slice(0, 19).replace('T', '_').replace(/:/g, '-');
}

function getColumnWidths(frame: DataFrame, fields: Field[]): Array<{ wch: number }> {
  return fields.map((field) => {
    let width = getFieldDisplayName(field, frame).length;
    const sampleCount = Math.min(frame.length, COLUMN_WIDTH_SAMPLE_ROWS);
    for (let rowIndex = 0; rowIndex < sampleCount; rowIndex++) {
      width = Math.max(width, String(getCellValue(field, rowIndex)).length);
    }
    return { wch: Math.max(12, Math.min(width + 2, 60)) };
  });
}

function yieldToBrowser(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

export async function exportDataFrameToExcel(
  frame: DataFrame,
  panelTitle: string,
  sortBy: TableSortByFieldState[] = [],
  onProgress?: ExcelExportProgress
): Promise<void> {
  const fields = getVisibleFields(frame);
  if (!fields.length) {
    return;
  }

  const workbook = XLSX.utils.book_new();
  const rowIndexes = getSortedRowIndexes(frame, fields, sortBy);
  const partCount = Math.max(1, Math.ceil(frame.length / MAX_DATA_ROWS_PER_SHEET));
  const columnWidths = getColumnWidths(frame, fields);
  let completedRows = 0;

  for (let part = 0; part < partCount; part++) {
    const partStart = part * MAX_DATA_ROWS_PER_SHEET;
    const partEnd = Math.min(frame.length, partStart + MAX_DATA_ROWS_PER_SHEET);
    const worksheet = XLSX.utils.aoa_to_sheet([fields.map((field) => getFieldDisplayName(field, frame))]);

    for (let chunkStart = partStart; chunkStart < partEnd; chunkStart += SHEET_BUILD_CHUNK_SIZE) {
      const chunkEnd = Math.min(partEnd, chunkStart + SHEET_BUILD_CHUNK_SIZE);
      const rows: Array<Array<string | number | boolean>> = [];
      for (let rowPosition = chunkStart; rowPosition < chunkEnd; rowPosition++) {
        const rowIndex = rowIndexes ? rowIndexes[rowPosition] : rowPosition;
        rows.push(fields.map((field) => getCellValue(field, rowIndex)));
      }
      XLSX.utils.sheet_add_aoa(worksheet, rows, { origin: -1 });
      completedRows += rows.length;
      onProgress?.(completedRows, frame.length);
      await yieldToBrowser();
    }

    worksheet['!cols'] = columnWidths;
    worksheet['!autofilter'] = {
      ref: XLSX.utils.encode_range({
        s: { c: 0, r: 0 },
        e: { c: fields.length - 1, r: partEnd - partStart },
      }),
    };
    XLSX.utils.book_append_sheet(workbook, worksheet, sanitizeSheetName(panelTitle, part, partCount));
  }

  onProgress?.(frame.length, frame.length);
  XLSX.writeFile(workbook, `${sanitizeFileName(panelTitle)}-${getTimestamp()}.xlsx`, { compression: true });
}
