import { DataFrame, Field, formattedValueToString, getFieldDisplayName } from '@grafana/data';
import { TableFieldOptions, TableSortByFieldState } from '@grafana/ui/src/components/Table/types';
import * as XLSX from 'xlsx';

const DEFAULT_FILE_NAME = 'grafana-table';
const DEFAULT_SHEET_NAME = 'Table';

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

function getRowIndexes(frame: DataFrame, fields: Field[], sortBy: TableSortByFieldState[]): number[] {
  const indexes = Array.from({ length: frame.length }, (_, index) => index);

  if (!sortBy.length) {
    return indexes;
  }

  const sortFields = sortBy
    .map((sort) => ({
      field: fields.find((field) => getFieldDisplayName(field, frame) === sort.displayName),
      desc: Boolean(sort.desc),
    }))
    .filter((sort): sort is { field: Field; desc: boolean } => Boolean(sort.field));

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

function sanitizeSheetName(title: string): string {
  const sanitized = title.replace(/[\\/?*:[\]]+/g, '_').trim();
  return (sanitized || DEFAULT_SHEET_NAME).slice(0, 31);
}

function getTimestamp(): string {
  return new Date().toISOString().slice(0, 19).replace('T', '_').replace(/:/g, '-');
}

function getColumnWidths(rows: Array<Array<string | number | boolean>>): Array<{ wch: number }> {
  const header = rows[0] || [];

  return header.map((_, columnIndex) => {
    const width = rows.reduce((current, row) => {
      const value = row[columnIndex];
      return Math.max(current, String(value === undefined ? '' : value).length);
    }, 0);

    return { wch: Math.max(12, Math.min(width + 2, 60)) };
  });
}

export function exportDataFrameToExcel(
  frame: DataFrame,
  panelTitle: string,
  sortBy: TableSortByFieldState[] = []
): void {
  const fields = getVisibleFields(frame);
  if (!fields.length) {
    return;
  }

  const rows: Array<Array<string | number | boolean>> = [fields.map((field) => getFieldDisplayName(field, frame))];
  const rowIndexes = getRowIndexes(frame, fields, sortBy);

  for (const rowIndex of rowIndexes) {
    rows.push(fields.map((field) => getCellValue(field, rowIndex)));
  }

  const worksheet = XLSX.utils.aoa_to_sheet(rows);
  worksheet['!cols'] = getColumnWidths(rows);
  worksheet['!autofilter'] = {
    ref: XLSX.utils.encode_range({ s: { c: 0, r: 0 }, e: { c: fields.length - 1, r: rows.length - 1 } }),
  };

  const workbook = XLSX.utils.book_new();
  XLSX.utils.book_append_sheet(workbook, worksheet, sanitizeSheetName(panelTitle));
  XLSX.writeFile(workbook, `${sanitizeFileName(panelTitle)}-${getTimestamp()}.xlsx`, { compression: true });
}
