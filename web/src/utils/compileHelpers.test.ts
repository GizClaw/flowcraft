import { describe, it, expect } from 'vitest';
import { parseCompileResult, parseDryRunResult, countDryRunIssues } from './compileHelpers';
import type { CompileResult, DryRunResult } from '../types/app';

describe('parseCompileResult', () => {
  it('returns empty maps when result has no errors or warnings', () => {
    const result: CompileResult = { success: true };
    const { errors, warnings } = parseCompileResult(result);
    expect(errors.size).toBe(0);
    expect(warnings.size).toBe(0);
  });

  it('groups errors by node_id', () => {
    const result: CompileResult = {
      success: false,
      errors: [
        { code: 'missing_config', message: 'missing model', node_ids: ['node-1', 'node-2'] },
        { code: 'invalid_edge', message: 'bad edge', node_ids: ['node-1'] },
      ],
    };
    const { errors } = parseCompileResult(result);
    expect(errors.get('node-1')).toEqual(['missing model', 'bad edge']);
    expect(errors.get('node-2')).toEqual(['missing model']);
  });

  it('groups warnings by node_id', () => {
    const result: CompileResult = {
      success: true,
      warnings: [
        { code: 'unused_var', message: 'unused variable x', node_ids: ['node-3'] },
        { code: 'deprecated', message: 'deprecated node type', node_ids: ['node-3', 'node-4'] },
      ],
    };
    const { warnings } = parseCompileResult(result);
    expect(warnings.get('node-3')).toEqual(['unused variable x', 'deprecated node type']);
    expect(warnings.get('node-4')).toEqual(['deprecated node type']);
  });

  it('handles warnings without node_ids (global warnings)', () => {
    const result: CompileResult = {
      success: true,
      warnings: [{ code: 'global', message: 'some global warning' }],
    };
    const { warnings } = parseCompileResult(result);
    expect(warnings.size).toBe(0);
  });

  it('handles mixed errors and warnings', () => {
    const result: CompileResult = {
      success: false,
      errors: [{ code: 'e1', message: 'error msg', node_ids: ['n1'] }],
      warnings: [{ code: 'w1', message: 'warn msg', node_ids: ['n1'] }],
    };
    const { errors, warnings } = parseCompileResult(result);
    expect(errors.get('n1')).toEqual(['error msg']);
    expect(warnings.get('n1')).toEqual(['warn msg']);
  });
});

describe('parseDryRunResult', () => {
  const INVALID_LABEL = 'Node validation failed';

  it('returns empty maps when node_results is undefined', () => {
    const result: DryRunResult = { valid: true };
    const { errors, warnings } = parseDryRunResult(result, INVALID_LABEL);
    expect(errors.size).toBe(0);
    expect(warnings.size).toBe(0);
  });

  it('returns empty maps when all nodes are valid with no warnings', () => {
    const result: DryRunResult = {
      valid: true,
      node_results: [
        { node_id: 'n1', valid: true },
        { node_id: 'n2', valid: true },
      ],
    };
    const { errors, warnings } = parseDryRunResult(result, INVALID_LABEL);
    expect(errors.size).toBe(0);
    expect(warnings.size).toBe(0);
  });

  it('marks invalid nodes in errors map', () => {
    const result: DryRunResult = {
      valid: false,
      node_results: [
        { node_id: 'n1', valid: false },
        { node_id: 'n2', valid: true },
      ],
    };
    const { errors } = parseDryRunResult(result, INVALID_LABEL);
    expect(errors.get('n1')).toEqual([INVALID_LABEL]);
    expect(errors.has('n2')).toBe(false);
  });

  it('collects per-node warnings into warnings map', () => {
    const result: DryRunResult = {
      valid: true,
      node_results: [
        { node_id: 'n1', valid: true, warnings: ['warn A', 'warn B'] },
        { node_id: 'n2', valid: true, warnings: ['warn C'] },
      ],
    };
    const { warnings } = parseDryRunResult(result, INVALID_LABEL);
    expect(warnings.get('n1')).toEqual(['warn A', 'warn B']);
    expect(warnings.get('n2')).toEqual(['warn C']);
  });

  it('handles nodes that are both invalid and have warnings', () => {
    const result: DryRunResult = {
      valid: false,
      node_results: [
        { node_id: 'n1', valid: false, warnings: ['also a warning'] },
      ],
    };
    const { errors, warnings } = parseDryRunResult(result, INVALID_LABEL);
    expect(errors.get('n1')).toEqual([INVALID_LABEL]);
    expect(warnings.get('n1')).toEqual(['also a warning']);
  });
});

describe('countDryRunIssues', () => {
  it('returns 0 for clean result', () => {
    const result: DryRunResult = { valid: true };
    expect(countDryRunIssues(result)).toBe(0);
  });

  it('returns 0 when all nodes valid and no warning groups', () => {
    const result: DryRunResult = {
      valid: true,
      node_results: [{ node_id: 'n1', valid: true }],
    };
    expect(countDryRunIssues(result)).toBe(0);
  });

  it('counts invalid nodes', () => {
    const result: DryRunResult = {
      valid: false,
      node_results: [
        { node_id: 'n1', valid: false },
        { node_id: 'n2', valid: false },
        { node_id: 'n3', valid: true },
      ],
    };
    expect(countDryRunIssues(result)).toBe(2);
  });

  it('counts warning groups', () => {
    const result: DryRunResult = {
      valid: true,
      warnings: [
        { code: 'w1', message: 'warn 1' },
        { code: 'w2', message: 'warn 2' },
      ],
    };
    expect(countDryRunIssues(result)).toBe(2);
  });

  it('sums invalid nodes and warning groups', () => {
    const result: DryRunResult = {
      valid: false,
      node_results: [{ node_id: 'n1', valid: false }],
      warnings: [{ code: 'w1', message: 'warn' }],
    };
    expect(countDryRunIssues(result)).toBe(2);
  });
});
