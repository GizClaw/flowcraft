export type VariableType = 'string' | 'number' | 'integer' | 'boolean' | 'array' | 'object' | 'file' | 'any';

export type VariableScope = 'input' | 'board' | 'config' | 'env';

export interface Variable {
  name: string;
  type: VariableType;
  required?: boolean;
  description?: string;
  default_value?: unknown;
  enum_values?: string[];
  items?: Variable;
  properties?: Variable[];
}

export interface VariableSchema {
  variables: Variable[];
}

export interface VariableRef {
  scope: VariableScope;
  name: string;
  raw: string;
}

export interface ValidationError {
  field: string;
  message: string;
}
