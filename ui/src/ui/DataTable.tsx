import { useMemo, useState, type ReactNode } from 'react';
import {
  LinearProgress,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  TableSortLabel,
} from '@mui/material';
import { EmptyState } from './EmptyState';

export interface Column<T> {
  id: string;
  header: ReactNode;
  /** Render the cell. Defaults to String(sortValue(row)). */
  cell?: (row: T) => ReactNode;
  /** Value used for sorting. Presence makes the column sortable. */
  sortValue?: (row: T) => string | number | null | undefined;
  align?: 'left' | 'right' | 'center';
  width?: number | string;
  /** Monospace cell, for ids and hashes. */
  mono?: boolean;
}

export interface DataTableProps<T> {
  columns: Column<T>[];
  rows: T[];
  getRowId: (row: T) => string;
  onRowClick?: (row: T) => void;
  /** Highlights the row with this id (the one open in a DetailPanel). */
  selectedId?: string | null;
  defaultSort?: { columnId: string; direction: 'asc' | 'desc' };
  loading?: boolean;
  /** Shown when rows is empty and not loading. */
  empty?: ReactNode;
  emptyTitle?: string;
  emptyDescription?: ReactNode;
}

type Dir = 'asc' | 'desc';

function compare(a: string | number | null | undefined, b: string | number | null | undefined) {
  if (a == null && b == null) return 0;
  if (a == null) return 1; // nulls last
  if (b == null) return -1;
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: 'base' });
}

export function DataTable<T>({
  columns,
  rows,
  getRowId,
  onRowClick,
  selectedId,
  defaultSort,
  loading,
  empty,
  emptyTitle = 'Nothing here yet',
  emptyDescription,
}: DataTableProps<T>) {
  const [sort, setSort] = useState<{ columnId: string; direction: Dir } | null>(
    defaultSort ?? null,
  );

  const sorted = useMemo(() => {
    if (!sort) return rows;
    const col = columns.find((c) => c.id === sort.columnId);
    const sv = col?.sortValue;
    if (!sv) return rows;
    const sign = sort.direction === 'asc' ? 1 : -1;
    return [...rows].sort((x, y) => sign * compare(sv(x), sv(y)));
  }, [rows, columns, sort]);

  const toggle = (id: string) =>
    setSort((s) =>
      s && s.columnId === id
        ? { columnId: id, direction: s.direction === 'asc' ? 'desc' : 'asc' }
        : { columnId: id, direction: 'asc' },
    );

  return (
    <TableContainer>
      <Table>
        <TableHead>
          <TableRow>
            {columns.map((c) => (
              <TableCell
                key={c.id}
                align={c.align}
                sx={{ width: c.width }}
                sortDirection={sort?.columnId === c.id ? sort.direction : false}
              >
                {c.sortValue ? (
                  <TableSortLabel
                    active={sort?.columnId === c.id}
                    direction={sort?.columnId === c.id ? sort.direction : 'asc'}
                    onClick={() => toggle(c.id)}
                  >
                    {c.header}
                  </TableSortLabel>
                ) : (
                  c.header
                )}
              </TableCell>
            ))}
          </TableRow>
        </TableHead>
        <TableBody>
          {sorted.map((row) => {
            const id = getRowId(row);
            return (
              <TableRow
                key={id}
                hover={!!onRowClick}
                selected={selectedId === id}
                onClick={onRowClick ? () => onRowClick(row) : undefined}
                onKeyDown={
                  onRowClick
                    ? (e) => {
                        if (e.key === 'Enter' || e.key === ' ') {
                          e.preventDefault();
                          onRowClick(row);
                        }
                      }
                    : undefined
                }
                tabIndex={onRowClick ? 0 : undefined}
                sx={{
                  cursor: onRowClick ? 'pointer' : undefined,
                  '&:focus-visible': { outline: '2px solid', outlineColor: 'primary.main', outlineOffset: -2 },
                }}
              >
                {columns.map((c) => (
                  <TableCell
                    key={c.id}
                    align={c.align}
                    sx={c.mono ? { fontFamily: (t) => t.gwag.mono, fontSize: '0.75rem' } : undefined}
                  >
                    {c.cell ? c.cell(row) : String(c.sortValue?.(row) ?? '')}
                  </TableCell>
                ))}
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
      {loading && <LinearProgress />}
      {!loading && sorted.length === 0 &&
        (empty ?? <EmptyState compact title={emptyTitle} description={emptyDescription} />)}
    </TableContainer>
  );
}
