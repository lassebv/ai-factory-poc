package swarm

// cpuBudget64MiB bounds CPU for CPU-01. Calibrated 2026-11-08 on the M3
// implementation (0.45 s on an M-series Mac); the budget is ~3x that, a
// regression guard rather than a target.
const cpuBudget64MiB = 1.5

// pipelineSlack allows for the fake peer counting a request before it has
// finished sending the previous block (measurement artifact, not torrentd).
const pipelineSlack = 6
