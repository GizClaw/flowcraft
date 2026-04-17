import type { VariableRef, VariableScope } from '../types/variable';

const REF_PATTERN = /\$\{(\w+)\.(\w+)\}/g;

export function extractRefs(text: string): VariableRef[] {
  const refs: VariableRef[] = [];
  let match;
  while ((match = REF_PATTERN.exec(text)) !== null) {
    refs.push({ scope: match[1] as VariableScope, name: match[2], raw: match[0] });
  }
  return refs;
}

export function validateRefs(text: string, availableScopes: Record<string, string[]>): string[] {
  const refs = extractRefs(text);
  const errors: string[] = [];
  for (const ref of refs) {
    const scopeVars = availableScopes[ref.scope];
    if (!scopeVars) {
      errors.push(`Unknown scope: ${ref.scope} in ${ref.raw}`);
    } else if (!scopeVars.includes(ref.name)) {
      errors.push(`Unknown variable: ${ref.name} in scope ${ref.scope}`);
    }
  }
  return errors;
}

export function getAvailableRefs(scope: VariableScope, variables: string[]): string[] {
  return variables.map((v) => `\${${scope}.${v}}`);
}
