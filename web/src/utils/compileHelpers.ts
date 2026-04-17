import type { CompileResult, DryRunResult } from '../types/app';

export interface NodeDiagnostics {
  errors: Map<string, string[]>;
  warnings: Map<string, string[]>;
}

/**
 * Parse CompileResult into per-node error/warning maps keyed by node_id.
 */
export function parseCompileResult(result: CompileResult): NodeDiagnostics {
  const errors = new Map<string, string[]>();
  const warnings = new Map<string, string[]>();

  const collect = (map: Map<string, string[]>, nodeIds: string[] | undefined, message: string) => {
    for (const nid of nodeIds ?? []) {
      map.set(nid, [...(map.get(nid) || []), message]);
    }
  };

  result.errors?.forEach((e) => collect(errors, e.node_ids, e.message));
  result.warnings?.forEach((w) => collect(warnings, w.node_ids, w.message));

  return { errors, warnings };
}

/**
 * Parse DryRunResult into per-node error/warning maps.
 * Nodes with valid=false get an entry in errors with `invalidLabel` as message.
 * Per-node warnings are collected into the warnings map.
 */
export function parseDryRunResult(result: DryRunResult, invalidLabel: string): NodeDiagnostics {
  const errors = new Map<string, string[]>();
  const warnings = new Map<string, string[]>();

  result.node_results?.forEach((nr) => {
    if (!nr.valid) {
      errors.set(nr.node_id, [...(errors.get(nr.node_id) || []), invalidLabel]);
    }
    nr.warnings?.forEach((w) => {
      warnings.set(nr.node_id, [...(warnings.get(nr.node_id) || []), w]);
    });
  });

  return { errors, warnings };
}

/**
 * Count total issues (invalid nodes + warning groups) from a DryRunResult.
 */
export function countDryRunIssues(result: DryRunResult): number {
  const invalidCount = result.node_results?.filter((nr) => !nr.valid).length ?? 0;
  const warnCount = result.warnings?.length ?? 0;
  return invalidCount + warnCount;
}
