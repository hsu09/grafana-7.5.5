import { DataFrame, Field, formattedValueToString, getFieldDisplayName } from '@grafana/data';
import { TableFieldOptions } from '@grafana/ui/src/components/Table/types';

export const TABLE_TEXT_ROW_LIMIT = 10000;

export interface PlainTextTable {
  text: string;
  rowCount: number;
  truncated: boolean;
}

function getVisibleFields(frame: DataFrame): Field[] {
  return frame.fields.filter((field) => {
    const options = (field.config.custom || {}) as TableFieldOptions;
    return !options.hidden;
  });
}

function getCellText(field: Field, rowIndex: number): string {
  const value = field.values.get(rowIndex);
  if (value === null || value === undefined) {
    return '';
  }

  let text: string;
  if (field.display) {
    text = formattedValueToString(field.display(value));
  } else if (typeof value === 'string') {
    text = value;
  } else {
    try {
      text = typeof value === 'object' ? JSON.stringify(value) : String(value);
    } catch {
      text = String(value);
    }
  }

  // Keep the copied output tabular. The textarea still wraps long values
  // visually, while tabs continue to paste cleanly into Excel or a text file.
  return text.replace(/[\r\n]+/g, ' ').replace(/\t/g, ' ');
}

export function buildPlainTextTable(frame: DataFrame, maxRows = TABLE_TEXT_ROW_LIMIT): PlainTextTable {
  const fields = getVisibleFields(frame);
  const rowCount = Math.min(frame.length, Math.max(0, maxRows));
  const lines: string[] = [fields.map((field) => getFieldDisplayName(field, frame)).join('\t')];

  for (let rowIndex = 0; rowIndex < rowCount; rowIndex++) {
    lines.push(fields.map((field) => getCellText(field, rowIndex)).join('\t'));
  }

  return {
    text: lines.join('\n'),
    rowCount,
    truncated: frame.length > rowCount,
  };
}
