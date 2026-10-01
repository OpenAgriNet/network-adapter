# Grievance capability

PMFBY and PM-KISAN grievances over one capability. Every call is synchronous; all
transformation is JSONata, in the same shape as the weather and mandi mappings under
`config/mappings/`.

| Doc | What it is |
|---|---|
| [Flow of execution](grievance-usecase.md) | Start here. The actions, the payloads, what each side sends |
| [Implementation](grievance-implementation.md) | The adapter's side — mappings, guards, prerequisites, what is enforced where |
| [Upstream contracts](grievance-upstream-contracts.md) | What the two portals actually accept and return. The reference the rest is derived from |
| [The `/support` variant](grievance-support-variant.md) | Why the grievance is lodged with `/support` rather than `confirm`. Adopted |
| [Design](grievance-design.md) | The earlier design note |

The schema packs these docs describe live in the `network-specs` repository, under
`api-schemas/PMFBYGrievance/v0.1` and `api-schemas/PMKISANGrievance/v0.1`.

`grievance-flow.excalidraw` is the editable source for the sequence diagram;
`grievance-flow.svg` and `grievance-flow.png` are generated from it and should both be
re-exported after any change.
