import type { Variable, ValidationError } from '../../types/variable';
import FieldWrapper from './FieldWrapper';
import JsonEditor from './JsonEditor';

interface Props {
  variable: Variable;
  value: unknown;
  onChange: (value: unknown) => void;
  error?: ValidationError;
}

export default function ArrayField({ variable, value, onChange, error }: Props) {
  return (
    <FieldWrapper variable={variable} error={error}>
      <JsonEditor value={value ?? variable.default_value ?? []} onChange={onChange} />
    </FieldWrapper>
  );
}
