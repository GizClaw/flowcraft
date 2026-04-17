export interface SelectOption {
  label: string;
  value: string;
}

export interface FieldSchema {
  key: string;
  label: string;
  type: 'text' | 'textarea' | 'number' | 'select' | 'boolean' | 'json' | 'variable_ref';
  required?: boolean;
  placeholder?: string;
  default_value?: unknown;
  options?: SelectOption[];
}

export interface PortSchema {
  name: string;
  type: string;
  required?: boolean;
  description?: string;
}

export interface NodeSchema {
  type: string;
  label: string;
  icon: string;
  color: string;
  category: string;
  description: string;
  fields: FieldSchema[];
  input_ports?: PortSchema[];
  output_ports?: PortSchema[];
  deprecated?: boolean;
}

export function deriveColorClasses(color: string): { bg: string; border: string; text: string; darkBg: string } {
  const map: Record<string, { bg: string; border: string; text: string; darkBg: string }> = {
    blue:    { bg: 'bg-blue-50',    border: 'border-blue-400',    text: 'text-blue-700',    darkBg: 'dark:bg-blue-950' },
    green:   { bg: 'bg-green-50',   border: 'border-green-400',   text: 'text-green-700',   darkBg: 'dark:bg-green-950' },
    orange:  { bg: 'bg-orange-50',  border: 'border-orange-400',  text: 'text-orange-700',  darkBg: 'dark:bg-orange-950' },
    purple:  { bg: 'bg-purple-50',  border: 'border-purple-400',  text: 'text-purple-700',  darkBg: 'dark:bg-purple-950' },
    pink:    { bg: 'bg-pink-50',    border: 'border-pink-400',    text: 'text-pink-700',    darkBg: 'dark:bg-pink-950' },
    cyan:    { bg: 'bg-cyan-50',    border: 'border-cyan-400',    text: 'text-cyan-700',    darkBg: 'dark:bg-cyan-950' },
    indigo:  { bg: 'bg-indigo-50',  border: 'border-indigo-400',  text: 'text-indigo-700',  darkBg: 'dark:bg-indigo-950' },
    yellow:  { bg: 'bg-yellow-50',  border: 'border-yellow-400',  text: 'text-yellow-700',  darkBg: 'dark:bg-yellow-950' },
    teal:    { bg: 'bg-teal-50',    border: 'border-teal-400',    text: 'text-teal-700',    darkBg: 'dark:bg-teal-950' },
    rose:    { bg: 'bg-rose-50',    border: 'border-rose-400',    text: 'text-rose-700',    darkBg: 'dark:bg-rose-950' },
    emerald: { bg: 'bg-emerald-50', border: 'border-emerald-400', text: 'text-emerald-700', darkBg: 'dark:bg-emerald-950' },
    amber:   { bg: 'bg-amber-50',   border: 'border-amber-400',   text: 'text-amber-700',   darkBg: 'dark:bg-amber-950' },
    lime:    { bg: 'bg-lime-50',    border: 'border-lime-400',    text: 'text-lime-700',    darkBg: 'dark:bg-lime-950' },
    red:     { bg: 'bg-red-50',     border: 'border-red-400',     text: 'text-red-700',     darkBg: 'dark:bg-red-950' },
    violet:  { bg: 'bg-violet-50',  border: 'border-violet-400',  text: 'text-violet-700',  darkBg: 'dark:bg-violet-950' },
    sky:     { bg: 'bg-sky-50',     border: 'border-sky-400',     text: 'text-sky-700',     darkBg: 'dark:bg-sky-950' },
    gray:    { bg: 'bg-gray-50',    border: 'border-gray-400',    text: 'text-gray-700',    darkBg: 'dark:bg-gray-950' },
  };
  return map[color] || { bg: 'bg-gray-50', border: 'border-gray-300', text: 'text-gray-700', darkBg: 'dark:bg-gray-950' };
}
