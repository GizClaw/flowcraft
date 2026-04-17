import type { Variable, ValidationError } from '../../types/variable';
import FieldWrapper from './FieldWrapper';
import VariableField from './VariableField';
import JsonEditor from './JsonEditor';

interface Props {
  variable: Variable;
  value: unknown;
  onChange: (value: unknown) => void;
  error?: ValidationError;
}

export default function ObjectField({ variable, value, onChange, error }: Props) {
  if (variable.properties && variable.properties.length > 0) {
    const obj = (typeof value === 'object' && value !== null && !Array.isArray(value) ? value : {}) as Record<string, unknown>;
    return (
      <FieldWrapper variable={variable} error={error}>
        <div className="pl-3 border-l-2 border-gray-200 dark:border-gray-700 space-y-3">
          {variable.properties.map((prop) => (
            <VariableField
              key={prop.name}
              variable={prop}
              value={obj[prop.name]}
              onChange={(v) => onChange({ ...obj, [prop.name]: v })}
            />
          ))}
        </div>
      </FieldWrapper>
    );
  }

  return (
    <FieldWrapper variable={variable} error={error}>
      <JsonEditor value={value ?? variable.default_value ?? {}} onChange={onChange} />
    </FieldWrapper>
  );
}
