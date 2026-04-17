import type { Variable, ValidationError } from '../../types/variable';
import FieldWrapper from './FieldWrapper';

interface Props {
  variable: Variable;
  value: unknown;
  onChange: (value: unknown) => void;
  error?: ValidationError;
}

export default function EnumField({ variable, value, onChange, error }: Props) {
  return (
    <FieldWrapper variable={variable} error={error}>
      <select
        value={String(value ?? variable.default_value ?? '')}
        onChange={(e) => onChange(e.target.value)}
        className="w-full px-3 py-2 text-sm rounded-lg border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800"
      >
        <option value="">Select...</option>
        {variable.enum_values?.map((opt) => <option key={opt} value={opt}>{opt}</option>)}
      </select>
    </FieldWrapper>
  );
}
