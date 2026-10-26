# Architecture Decision Records

One file per decision, numbered, never edited after acceptance: a decision
that turns out wrong is superseded by a new record, not rewritten. Code
comments stay short and point here for the reasoning.

Start from [`0000-template.md`](0000-template.md).

| # | Decision |
|---|---|
| [0001](0001-provider-envelopes-in-common.md) | Provider envelopes live in common, with capability hooks |
| [0002](0002-an-action-served-by-another-participant.md) | An action served by another binding is named by its binding key |
| [0003](0003-input-validation-in-capability-packs.md) | Input validation lives in the capability packs, not the mappings |
| [0004](0004-upstream-refusals-400-or-502.md) | An upstream refusal is a 400 when its reason is known, else a 502 |
| [0005](0005-a-follow-up-read-after-the-call.md) | A follow-up read adds a second answer to the first |
