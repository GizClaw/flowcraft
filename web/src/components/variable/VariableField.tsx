import type { Variable, ValidationError } from '../../types/variable';
import StringField from './StringField';
import NumberField from './NumberField';
import BooleanField from './BooleanField';
import EnumField from './EnumField';
import FileField from './FileField';
import ArrayField from './ArrayField';
import ObjectField from './ObjectField';

interface Props {
  variable: Variable;
  value: unknown;
  onChange: (value: unknown) => void;
  error?: ValidationError;
}

export default function VariableField({ variable, value, onChange, error }: Props) {
  if (variable.enum_values && variable.enum_values.length > 0) {
    return <EnumField variable={variable} value={value} onChange={onChange} error={error} />;
  }

  switch (variable.type) {
    case 'boolean':
      return <BooleanField variable={variable} value={value} onChange={onChange} error={error} />;
    case 'number':
    case 'integer':
      return <NumberField variable={variable} value={value} onChange={onChange} error={error} />;
    case 'file':
      return <FileField variable={variable} value={value} onChange={onChange} error={error} />;
    case 'array':
      return <ArrayField variable={variable} value={value} onChange={onChange} error={error} />;
    case 'object':
      return <ObjectField variable={variable} value={value} onChange={onChange} error={error} />;
    default:
      return <StringField variable={variable} value={value} onChange={onChange} error={error} />;
  }
}
