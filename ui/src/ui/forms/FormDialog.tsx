import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
  FormControl,
  FormControlLabel,
  FormHelperText,
  InputLabel,
  MenuItem,
  Select,
  Stack,
  Switch,
  TextField,
} from '@mui/material';
import { useForm } from '@tanstack/react-form';
import { mapFieldErrors, type GraphQLErrorLike } from './fieldErrors';

// Minimal view of the TanStack form instance used by the field
// components. FormDialog owns the typed instance; fields address values
// by string name, so a loose structural type is enough here.
interface LooseField {
  state: { value: unknown; meta: { errors: unknown[] } };
  handleChange: (v: unknown) => void;
  handleBlur: () => void;
}
interface LooseForm {
  Field: (props: { name: string; children: (field: LooseField) => ReactNode }) => ReactNode;
}

interface FormCtx {
  form: LooseForm;
  serverErrors: Record<string, string>;
  clearServerError: (name: string) => void;
  register: (name: string) => () => void;
  submitting: boolean;
}
const Ctx = createContext<FormCtx | null>(null);

function useFormCtx(): FormCtx {
  const v = useContext(Ctx);
  if (!v) throw new Error('Form fields must be rendered inside <FormDialog>');
  return v;
}

/** Error text for a field: server error first, then TanStack validation errors. */
function useFieldError(name: string, field: LooseField): string | undefined {
  const { serverErrors } = useFormCtx();
  const local = field.state.meta.errors.find((e): e is string => typeof e === 'string');
  return serverErrors[name] ?? local;
}

function toText(v: unknown): string {
  return typeof v === 'string' || typeof v === 'number' || typeof v === 'boolean' ? String(v) : '';
}

function useRegister(name: string) {
  const { register } = useFormCtx();
  useEffect(() => register(name), [register, name]);
}

export interface FormTextFieldProps {
  /** TanStack field name: "name", "inner.name", "items[0].name". */
  name: string;
  label: string;
  helperText?: ReactNode;
  multiline?: boolean;
  rows?: number;
  type?: 'text' | 'number' | 'password' | 'url';
  placeholder?: string;
  required?: boolean;
  autoFocus?: boolean;
  monospace?: boolean;
}

export function FormTextField({
  name,
  label,
  helperText,
  multiline,
  rows,
  type = 'text',
  placeholder,
  required,
  autoFocus,
  monospace,
}: FormTextFieldProps) {
  const { form, clearServerError, submitting } = useFormCtx();
  useRegister(name);
  return (
    <form.Field name={name}>
      {(field) => (
        <FieldInner name={name} field={field}>
          {(error) => (
            <TextField
              fullWidth
              label={label}
              type={type}
              multiline={multiline}
              minRows={rows}
              placeholder={placeholder}
              required={required}
              autoFocus={autoFocus}
              disabled={submitting}
              value={toText(field.state.value)}
              onChange={(e) => {
                clearServerError(name);
                field.handleChange(type === 'number' && e.target.value !== '' ? Number(e.target.value) : e.target.value);
              }}
              onBlur={field.handleBlur}
              error={!!error}
              helperText={error ?? helperText}
              slotProps={{
                input: monospace ? { sx: { fontFamily: (t) => t.gwag.mono, fontSize: '0.8125rem' } } : undefined,
              }}
            />
          )}
        </FieldInner>
      )}
    </form.Field>
  );
}

function FieldInner({
  name,
  field,
  children,
}: {
  name: string;
  field: LooseField;
  children: (error: string | undefined) => ReactNode;
}) {
  const error = useFieldError(name, field);
  return <>{children(error)}</>;
}

export interface FormSelectProps {
  name: string;
  label: string;
  options: { value: string; label: string }[];
  helperText?: ReactNode;
}

export function FormSelect({ name, label, options, helperText }: FormSelectProps) {
  const { form, clearServerError, submitting } = useFormCtx();
  useRegister(name);
  const labelId = `fs-${name}`;
  return (
    <form.Field name={name}>
      {(field) => (
        <FieldInner name={name} field={field}>
          {(error) => (
            <FormControl fullWidth size="small" error={!!error} disabled={submitting}>
              <InputLabel id={labelId}>{label}</InputLabel>
              <Select
                labelId={labelId}
                label={label}
                value={toText(field.state.value)}
                onChange={(e) => {
                  clearServerError(name);
                  field.handleChange(e.target.value);
                }}
                onBlur={field.handleBlur}
              >
                {options.map((o) => (
                  <MenuItem key={o.value} value={o.value}>
                    {o.label}
                  </MenuItem>
                ))}
              </Select>
              {(error ?? helperText) && <FormHelperText>{error ?? helperText}</FormHelperText>}
            </FormControl>
          )}
        </FieldInner>
      )}
    </form.Field>
  );
}

export interface FormSwitchProps {
  name: string;
  label: string;
  helperText?: ReactNode;
}

export function FormSwitch({ name, label, helperText }: FormSwitchProps) {
  const { form, clearServerError, submitting } = useFormCtx();
  useRegister(name);
  return (
    <form.Field name={name}>
      {(field) => (
        <FieldInner name={name} field={field}>
          {(error) => (
            <FormControl error={!!error}>
              <FormControlLabel
                label={label}
                disabled={submitting}
                control={
                  <Switch
                    checked={field.state.value === true}
                    onChange={(e) => {
                      clearServerError(name);
                      field.handleChange(e.target.checked);
                    }}
                  />
                }
              />
              {(error ?? helperText) && <FormHelperText>{error ?? helperText}</FormHelperText>}
            </FormControl>
          )}
        </FieldInner>
      )}
    </form.Field>
  );
}

export interface FormDialogProps<TValues extends Record<string, unknown>> {
  open: boolean;
  title: ReactNode;
  description?: ReactNode;
  submitLabel?: string;
  defaultValues: TValues;
  /**
   * Perform the mutation. Throw to report failure. If the thrown value
   * carries a GraphQL `errors` list (GraphQLRequestError), field errors
   * from `extensions.fields` are placed on the matching fields; the rest
   * show as a form-level alert. The dialog stays open on failure.
   */
  onSubmit: (values: TValues) => Promise<void> | void;
  onClose: () => void;
  /** Root arg segment stripped from server paths. Default "body". */
  fieldPrefix?: string;
  maxWidth?: 'xs' | 'sm' | 'md';
  children: ReactNode;
}

function errorsOf(e: unknown): GraphQLErrorLike[] {
  if (typeof e === 'object' && e !== null && 'errors' in e) {
    const list: unknown = e.errors;
    if (Array.isArray(list)) {
      const errs = list.filter(
        (x): x is GraphQLErrorLike =>
          typeof x === 'object' && x !== null && typeof (x as { message?: unknown }).message === 'string',
      );
      if (errs.length > 0) return errs;
    }
  }
  return [{ message: e instanceof Error ? e.message : String(e) }];
}

export function FormDialog<TValues extends Record<string, unknown>>(
  props: FormDialogProps<TValues>,
) {
  const { open, onClose, title, maxWidth = 'sm' } = props;
  // Dialog unmounts its body when closed, so each open starts with a fresh form.
  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth={maxWidth}>
      <DialogTitle>{title}</DialogTitle>
      <FormBody {...props} />
    </Dialog>
  );
}

function FormBody<TValues extends Record<string, unknown>>({
  description,
  submitLabel = 'Save',
  defaultValues,
  onSubmit,
  onClose,
  fieldPrefix,
  children,
}: FormDialogProps<TValues>) {
  const [serverErrors, setServerErrors] = useState<Record<string, string>>({});
  const [formErrors, setFormErrors] = useState<string[]>([]);
  const registered = useRef(new Map<string, number>());

  const form = useForm({
    defaultValues,
    onSubmit: async ({ value }) => {
      setServerErrors({});
      setFormErrors([]);
      try {
        await onSubmit(value);
        onClose();
      } catch (e) {
        const mapped = mapFieldErrors(errorsOf(e), {
          prefix: fieldPrefix,
          knownFields: registered.current.keys(),
        });
        setServerErrors(mapped.fields);
        setFormErrors(mapped.form);
      }
    },
  });

  const register = useCallback((name: string) => {
    const m = registered.current;
    m.set(name, (m.get(name) ?? 0) + 1);
    return () => {
      const n = (m.get(name) ?? 1) - 1;
      if (n <= 0) m.delete(name);
      else m.set(name, n);
    };
  }, []);

  const clearServerError = useCallback(
    (name: string) =>
      setServerErrors((s) => {
        if (!(name in s)) return s;
        const { [name]: _drop, ...rest } = s;
        return rest;
      }),
    [],
  );

  return (
    <form.Subscribe selector={(s) => s.isSubmitting}>
      {(submitting) => {
        const ctx: FormCtx = {
          form: form as unknown as LooseForm,
          serverErrors,
          clearServerError,
          register,
          submitting,
        };
        return (
          <Ctx.Provider value={ctx}>
            <form
              noValidate
              onSubmit={(e) => {
                e.preventDefault();
                e.stopPropagation();
                void form.handleSubmit();
              }}
            >
              <DialogContent>
                <Stack spacing={2} sx={{ pt: 1 }}>
                  {description && <DialogContentText>{description}</DialogContentText>}
                  {formErrors.length > 0 && (
                    <Alert severity="error">
                      {formErrors.map((m, i) => (
                        <div key={i}>{m}</div>
                      ))}
                    </Alert>
                  )}
                  {children}
                </Stack>
              </DialogContent>
              <DialogActions sx={{ px: 3, pb: 2 }}>
                <Button onClick={onClose} disabled={submitting} color="inherit">
                  Cancel
                </Button>
                <Button type="submit" variant="contained" disabled={submitting}>
                  {submitting ? 'Saving…' : submitLabel}
                </Button>
              </DialogActions>
            </form>
          </Ctx.Provider>
        );
      }}
    </form.Subscribe>
  );
}
