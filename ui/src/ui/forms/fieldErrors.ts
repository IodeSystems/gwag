// Maps GraphQL errors from the gateway onto form fields.
//
// Server shape (huma is the single source of validation; the UI does
// no client-side schema validation):
//
//   { message, extensions: { fields: [{ path: "body.items.1.name", message }] } }
//
// `path` is a dot-separated GraphQL argument path rooted at the arg
// name. Body fields: `body.name`, `body.inner.name`, `body.items.1.name`.
// Query/path/header params are the bare arg name (`limit`). A numeric
// segment is a list index. Form field names use TanStack Form syntax:
// `items[1].name`. The root arg segment (default `body`) is stripped;
// bare param names pass through unchanged, as do unknown paths.

export interface ServerFieldError {
  path: string;
  message: string;
}

export interface GraphQLErrorLike {
  message: string;
  extensions?: { fields?: unknown } & Record<string, unknown>;
}

export interface MappedErrors {
  /** Field name (TanStack syntax) -> message. Multiple messages are joined with "; ". */
  fields: Record<string, string>;
  /** Messages with no matching field. */
  form: string[];
}

export interface MapOptions {
  /** Root arg segment to strip from the front of every path. Default "body". */
  prefix?: string;
  /** If given, errors for names outside this set go to `form`, prefixed with the name. */
  knownFields?: Iterable<string>;
}

/** "body.items.1.name" -> "items[1].name". Returns null when nothing is left after stripping. */
export function pathToFieldName(path: string, prefix = 'body'): string | null {
  let segs = path.split('.').filter((s) => s !== '');
  if (prefix && segs[0] === prefix) segs = segs.slice(1);
  if (segs.length === 0) return null;
  let out = '';
  for (const s of segs) {
    if (/^\d+$/.test(s)) out += `[${s}]`;
    else out += out === '' ? s : `.${s}`;
  }
  return out;
}

function isFieldError(v: unknown): v is ServerFieldError {
  return (
    typeof v === 'object' &&
    v !== null &&
    typeof (v as ServerFieldError).path === 'string' &&
    typeof (v as ServerFieldError).message === 'string'
  );
}

export function mapFieldErrors(
  errors: readonly GraphQLErrorLike[],
  opts: MapOptions = {},
): MappedErrors {
  const known = opts.knownFields ? new Set(opts.knownFields) : null;
  const fields: Record<string, string> = {};
  const form: string[] = [];

  const addField = (name: string, message: string) => {
    fields[name] = name in fields ? `${fields[name]}; ${message}` : message;
  };

  for (const err of errors) {
    const raw = err.extensions?.fields;
    const list = Array.isArray(raw) ? raw.filter(isFieldError) : [];
    if (list.length === 0) {
      form.push(err.message);
      continue;
    }
    for (const fe of list) {
      const name = pathToFieldName(fe.path, opts.prefix);
      if (name === null) form.push(fe.message);
      else if (known && !known.has(name)) form.push(`${name}: ${fe.message}`);
      else addField(name, fe.message);
    }
  }
  return { fields, form };
}
