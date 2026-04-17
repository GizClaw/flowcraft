import type { VariableSchema } from '../../types/variable';
import type { ValidationError } from '../../types/variable';
import VariableField from './VariableField';

interface Props {
  schema: VariableSchema;
  values: Record<string, unknown>;
  onChange: (values: Record<string, unknown>) => void;
  errors?: ValidationError[];
}

export default function SchemaForm({ schema, values, onChange, errors = [] }: Props) {
  return (
    <div className="space-y-4">
      {schema.variables.map((v) => (
        <VariableField
          key={v.name}
          variable={v}
          value={values[v.name]}
          onChange={(val) => onChange({ ...values, [v.name]: val })}
          error={errors.find((e) => e.field === v.name)}
        />
      ))}
    </div>
  );
}
