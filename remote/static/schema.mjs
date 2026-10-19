import { z } from "./vendor/zod.mjs";

const integer = z.number().int();
const timestamp = z.iso.datetime({ offset: true });
const nonnegative = z.number().nonnegative();
export const schedulingModeSchema = z.literal(["fixed", "shadow", "adaptive"]);
const hostSchema = z.object({
  sampledAt: nonnegative, totalMemoryBytes: integer.positive(), availableMemoryBytes: integer.nonnegative(),
  cpuCores: integer.positive(), cpuBusyCores: nonnegative,
  memoryPressure: z.literal(["normal", "warning", "critical"]), swapUsedBytes: integer.nonnegative(),
  swapInBytesPerSecond: nonnegative, swapOutBytesPerSecond: nonnegative,
  diskFreeBytes: integer.nonnegative(), machine: z.string(), complete: z.boolean().optional(),
});
const resourcesSchema = z.object({
  prediction: z.object({
    memoryBytes: integer.nonnegative(), cpuCores: nonnegative, durationSeconds: nonnegative.nullable(),
    samples: integer.nonnegative(), confidence: z.literal(["unknown", "low", "medium", "high"]),
    source: z.literal(["variant", "family", "prior"]),
  }),
  observed: z.object({
    memoryBytes: integer.nonnegative(), peakMemoryBytes: integer.nonnegative(), cpuSeconds: nonnegative,
    cpuCores: nonnegative, durationSeconds: nonnegative, samples: integer.nonnegative(), complete: z.boolean(),
  }).nullable(),
  decision: z.object({ allowed: z.boolean(), reason: z.string() }).nullable(),
});
export const jobSchema = z.object({
  id: integer.nonnegative(), state: z.string().min(1), title: z.string(),
  project: z.string(), branch: z.string(), directory: z.string(),
  executable: z.string(), priority: integer, queuePosition: integer.positive().nullable(),
  submittedAt: timestamp.nullable(), startedAt: timestamp.nullable(),
  finishedAt: timestamp.nullable(), exitCode: integer.nullable(),
  logPath: z.string(), canReorder: z.boolean(), canStart: z.boolean(),
  canCancel: z.boolean(), phase: z.string(),
  resources: resourcesSchema.optional(),
});
export const snapshotSchema = z.object({
  updatedAt: timestamp,
  queue: z.object({
    status: z.string().min(1), limit: integer.min(1).max(8),
    running: integer.nonnegative(), waiting: integer.nonnegative(), legacyOccupied: z.boolean(),
  }),
  jobs: z.array(jobSchema).max(10000),
  scheduler: z.object({
    mode: schedulingModeSchema, available: z.boolean(), reason: z.string(),
    updatedAt: timestamp.nullable(), host: hostSchema.nullable(),
  }).optional(),
}).refine((snapshot) => new Set(snapshot.jobs.map((job) => job.id)).size === snapshot.jobs.length);
export const csrfSchema = z.object({ token: z.string().min(1).max(4096) });
export const resultSchema = z.object({ ok: z.literal(true) });
export const errorSchema = z.object({ error: z.string().min(1).max(4096) });
export const logSchema = z.object({ output: z.string().max(262144), truncated: z.boolean() });
export const waiting = (job) => job.state === "queued" || job.state === "waiting";
export const running = (job) => ["running", "starting", "cancelling"].includes(job.state);
export function eligible(job, action) {
  if (!job) return false;
  if (action === "start") return job.canStart && waiting(job);
  if (action === "next" || action === "last") return job.canReorder && waiting(job);
  return action === "cancel" && job.canCancel;
}
