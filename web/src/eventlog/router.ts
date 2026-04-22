import type { Envelope } from './types';

type Reducer<P = unknown> = (env: Envelope<P>) => void;

// EnvelopeRouter is the only allowed dispatcher for envelope-typed
// events on the frontend. Business stores register reducers per type;
// the router delegates each envelope to all registered reducers for its
// type. Unknown types are silently ignored so adding a new event in the
// backend does not require a frontend change before the relevant reducer
// is wired.
export class EnvelopeRouter {
  private reducers = new Map<string, Set<Reducer>>();

  on<P = unknown>(type: string, fn: Reducer<P>): () => void {
    let bucket = this.reducers.get(type);
    if (!bucket) {
      bucket = new Set();
      this.reducers.set(type, bucket);
    }
    bucket.add(fn as Reducer);
    return () => {
      bucket?.delete(fn as Reducer);
      if (bucket && bucket.size === 0) this.reducers.delete(type);
    };
  }

  dispatch(env: Envelope): void {
    const bucket = this.reducers.get(env.type);
    if (!bucket) return;
    bucket.forEach((fn) => {
      try {
        fn(env);
      } catch (err) {
        console.error('envelopeRouter: reducer threw', env.type, err);
      }
    });
  }

  // Test helper. Production code does not need to inspect the table.
  hasReducer(type: string): boolean {
    return this.reducers.has(type);
  }
}

export const envelopeRouter = new EnvelopeRouter();
