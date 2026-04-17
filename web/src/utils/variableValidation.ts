import type { Variable, VariableSchema, ValidationError } from '../types/variable';

export function validateValues(schema: VariableSchema | undefined, values: Record<string, unknown>): ValidationError[] {
  if (!schema?.variables) return [];
  const errors: ValidationError[] = [];

  for (const v of schema.variables) {
    const val = values[v.name];

    if (v.required && (val === undefined || val === null || val === '')) {
      errors.push({ field: v.name, message: `${v.name} is required` });
      continue;
    }

    if (val === undefined || val === null) continue;

    switch (v.type) {
      case 'number':
      case 'integer':
        if (typeof val !== 'number' && isNaN(Number(val))) {
          errors.push({ field: v.name, message: `${v.name} must be a number` });
        }
        if (v.type === 'integer' && typeof val === 'number' && !Number.isInteger(val)) {
          errors.push({ field: v.name, message: `${v.name} must be an integer` });
        }
        break;
      case 'boolean':
        if (typeof val !== 'boolean') {
          errors.push({ field: v.name, message: `${v.name} must be a boolean` });
        }
        break;
      case 'array':
        if (!Array.isArray(val)) {
          errors.push({ field: v.name, message: `${v.name} must be an array` });
        }
        break;
      case 'object':
        if (typeof val !== 'object' || Array.isArray(val)) {
          errors.push({ field: v.name, message: `${v.name} must be an object` });
        }
        break;
    }

    if (v.enum_values && v.enum_values.length > 0 && !v.enum_values.includes(String(val))) {
      errors.push({ field: v.name, message: `${v.name} must be one of: ${v.enum_values.join(', ')}` });
    }
  }

  return errors;
}

export function applyDefaults(schema: VariableSchema | undefined, values: Record<string, unknown>): Record<string, unknown> {
  if (!schema?.variables) return values;
  const result = { ...values };
  for (const v of schema.variables) {
    if (result[v.name] === undefined && v.default_value !== undefined) {
      result[v.name] = v.default_value;
    }
  }
  return result;
}

export function variableToFormField(v: Variable): { type: string; options?: { label: string; value: string }[] } {
  if (v.enum_values && v.enum_values.length > 0) {
    return { type: 'select', options: v.enum_values.map((e) => ({ label: e, value: e })) };
  }
  switch (v.type) {
    case 'string': return { type: 'text' };
    case 'number': case 'integer': return { type: 'number' };
    case 'boolean': return { type: 'boolean' };
    case 'array': case 'object': return { type: 'json' };
    case 'file': return { type: 'file' };
    default: return { type: 'text' };
  }
}
