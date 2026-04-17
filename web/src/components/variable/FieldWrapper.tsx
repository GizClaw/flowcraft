import type { ReactNode } from 'react';
import type { Variable, ValidationError } from '../../types/variable';

interface Props {
  variable: Variable;
  error?: ValidationError;
  children: ReactNode;
}

export default function FieldWrapper({ variable, error, children }: Props) {
  return (
    <div>
      <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
        {variable.name} {variable.required && <span className="text-red-400">*</span>}
      </label>
      {variable.description && <p className="text-xs text-gray-500 mb-1">{variable.description}</p>}
      {children}
      {error && <p className="text-xs text-red-500 mt-1">{error.message}</p>}
    </div>
  );
}
