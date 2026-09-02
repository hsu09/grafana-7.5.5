import { FieldType, MutableDataFrame } from '@grafana/data';
import { buildPlainTextTable } from './plainText';

describe('buildPlainTextTable', () => {
  it('keeps complete long values and produces tab-separated rows', () => {
    const frame = new MutableDataFrame({
      fields: [
        { name: 'queue', type: FieldType.string, values: ['debug', 'normal'] },
        {
          name: 'hosts',
          type: FieldType.string,
          values: ['host01 host02 host03', 'host11 host12'],
        },
      ],
    });

    expect(buildPlainTextTable(frame)).toEqual({
      text: 'queue\thosts\ndebug\thost01 host02 host03\nnormal\thost11 host12',
      rowCount: 2,
      truncated: false,
    });
  });

  it('hides configured fields, flattens embedded line breaks, and honors the row limit', () => {
    const frame = new MutableDataFrame({
      fields: [
        { name: 'queue', type: FieldType.string, values: ['debug', 'normal'] },
        {
          name: 'secret',
          type: FieldType.string,
          values: ['one', 'two'],
          config: { custom: { hidden: true } },
        },
        { name: 'hosts', type: FieldType.string, values: ['host01\nhost02', 'host11'] },
      ],
    });

    expect(buildPlainTextTable(frame, 1)).toEqual({
      text: 'queue\thosts\ndebug\thost01 host02',
      rowCount: 1,
      truncated: true,
    });
  });
});
