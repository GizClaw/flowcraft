import type { Variable, ValidationError } from '../../types/variable';
import FieldWrapper from './FieldWrapper';

interface Props {
  variable: Variable;
  value: unknown;
  onChange: (value: unknown) => void;
  error?: ValidationError;
}

export default function BooleanField({ variable, value, onChange, error }: Props) {
  return (
    <FieldWrapper variable={variable} error={error}>
      <label className="flex items-center gap-2">
        <input
          type="checkbox"
          checked={Boolean(value ?? variable.default_value)}
          onChange={(e) => onChange(e.target.checked)}
          className="rounded border-gray-300 text-indigo-600"
        />
        <span className="text-sm text-gray-600 dark:text-gray-400">{variable.name}</span>
      </label>
    </FieldWrapper>
  );
}
