import { describe, expect, it } from 'vitest';
import { computeAppendPlan } from './trim.js';

describe('computeAppendPlan', () => {
  it('places the first append at the origin', () => {
    const plan = computeAppendPlan({ trueDuration: 10, index: 0 });
    expect(plan.timestampOffset).toBe(0);
    expect(plan.appendWindowStart).toBe(0);
    expect(plan.appendWindowEnd).toBe(10);
  });

  it('starts the second append exactly at the first true end', () => {
    const plan = computeAppendPlan({ trueDuration: 10, index: 1 });
    expect(plan.timestampOffset).toBe(10);
    expect(plan.appendWindowStart).toBe(10);
    expect(plan.appendWindowEnd).toBe(20);
  });

  it('clips the encoder overshoot rather than the true content', () => {
    // The AAC half's container runs 10.054240s for 10s of real audio. The
    // window must end at the true content, not at the container duration.
    const plan = computeAppendPlan({ trueDuration: 10, index: 1 });
    expect(plan.appendWindowEnd).toBe(20);
    expect(plan.appendWindowEnd).toBeLessThan(10 + 10.054240);
  });
});
