import type { Variable, ValidationError } from '../../types/variable';
import FieldWrapper from './FieldWrapper';

interface Props {
  variable: Variable;
  value: unknown;
  onChange: (value: unknown) => void;
  error?: ValidationError;
}

export default function NumberField({ variable, value, onChange, error }: Props) {
  return (
    <FieldWrapper variable={variable} error={error}>
      <input
        type="number"
        value={String(value ?? variable.default_value ?? '')}
        onChange={(e) => onChange(e.target.value === '' ? undefined : Number(e.target.value))}
        step={variable.type === 'integer' ? 1 : 'any'}
        className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
      />
    </FieldWrapper>
  );
}
