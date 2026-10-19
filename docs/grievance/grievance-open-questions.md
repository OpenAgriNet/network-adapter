# Grievance and application status — what we need from the portals

Everything in this list blocks a decision we cannot make on our own. Each row says what we
need, why it matters, and what happens today without it. None of it needs a schema change
unless the row says so.

Reference: [grievance-usecase.md](grievance-usecase.md), which is where each of these is
marked open in context.

---

## 1. PMFBY (Amnex)

| # | What we need | Why | Today, without it |
|---|---|---|---|
| 1.1 | The real `TicketStatus` phrase list the portal returns | We map each phrase to one of seven `CaseStatusCode` values. We cannot map phrases we have never seen | Every unrecognised phrase falls to `UnderReview`, so a read only ever shows `Registered` or `UnderReview`. The other five codes are unreachable |
| 1.2 | The full category and sub-category table | We publish the desk's categories in the catalog so the farmer can pick one. We currently publish the only pair we have seen work, `3` / `10` | The catalog offers one choice. Every complaint is filed under that pair regardless of what it is about |
| 1.3 | Whether any pair other than `3` / `10` is accepted | Same reason. If `3`/`10` is genuinely the only valid pair, we will say so and stop treating it as a gap | The pack leaves the digit pattern open rather than fixing its length, because we are guessing |
| 1.4 | The real application-number format | The complaint is filed against an enrolment. We validate the shape before sending, so a typo is caught at the edge rather than upstream | No format check. A mistyped number reaches the portal |
| 1.5 | **Rotate the `NCIP_USER` password** | It was shared with us in plaintext inside a Postman collection, so it must be treated as disclosed | The credential in that collection should be assumed compromised until it is rotated |
| 1.6 | Confirm the production host | We have `https://pmfbydemo.amnex.co.in`, which is the demo | The registry record names the demo host. It cannot go to production as it stands |

**One observation to raise, not a question (1.7).** The lodge call accepts an application
number without proving the caller owns it, and application numbers have visible structure.
That is how the portal already works and this network does not make it worse, but it is
worth saying out loud. Any control PMFBY wants belongs on FGMS beside the lodge, not
borrowed from another service.

---

## 2. PM-KISAN

| # | What we need | Why | Today, without it |
|---|---|---|---|
| 2.1 | The real `GrievanceStatus` phrase list | Same as 1.1 — we map phrases to `CaseStatusCode` | Everything falls to `UnderReview` |
| 2.2 | Is an OTP single-use? | We assume it is, so every read opens its own `init` and sends a fresh SMS | A farmer who files and immediately checks gets two SMS. If one OTP can be reused inside its window, we can spare them the second |
| 2.3 | How many wrong OTPs are allowed, and what happens then | A caller building a retry screen needs to know whether the fourth wrong attempt locks the registration | Nobody knows. Neither the portal nor the v1 adapter counts attempts or records a lockout, so a retry loop is written blind |
| 2.4 | Is `Reg_No` always two letters followed by nine digits? | Same as 1.4 — we would validate the shape at the edge | We accept anything, and a typo reaches the portal |
| 2.5 | Can a case read return more than one reply on the same grievance? | Our `case` band carries one `reply` and one `repliedOn`. A thread needs a different shape, which is a schema change | We model one reply. If the portal threads, we would be showing the farmer only part of it |
| 2.6 | What should a farmer do when the SMS never arrives? | An `init` acknowledgement says the portal accepted the request, not that anything was delivered. There is no resend distinct from a fresh `init`, and no `sentTo` to show which number it went to | The app has nothing to tell the farmer except "try again", which sends another SMS to a number we cannot name |
| 2.7 | Confirm the production grievance host | We have a staging host; the production one is not recorded anywhere we can find | The registry record names staging |
| 2.8 | Confirm the chatbot OTP host | The host we have comes from the legacy v1 configuration, not from PM-KISAN | Unverified, and it is the host the OTP flow depends on |

---

## 3. What does not block us

Listed so nobody chases them:

- **No lifecycle closes today.** Neither portal documents a final status, so no case read
  returns `Resolved`, `Rejected` or `Closed`. That is a consequence of 1.1 and 2.1, not a
  separate question.
- **The portal's own message is never returned to the caller.** It may carry a stack
  trace, an internal hostname or a quoted-back credential. We log it redacted and return
  our own. No portal input needed.
- **Everything a portal returns about the farmer beyond what we publish is dropped.** Name,
  parentage, gender, phone and full address are not mapped. That is a decision already
  taken, not an open question.

---

## 4. What is open on our side

Not portal questions. Recorded here so the whole list is in one place.

| # | What | Who decides |
|---|---|---|
| 4.1 | PM-KISAN `init` host binding — a Participant carries one `baseUrl`, but `init` goes to the chatbot OTP service and the other two calls go to the grievance host. Either the chatbot call becomes plugin orchestration, like `/ChatbotOTPVerified` already is, or the registry grows a per-action host | Architecture, with the adapter team |
| 4.2 | `SchemaRegistry.schemaUrl` only accepts a `/schema/` path, and the packs live under `/api-schemas/`. The pattern has to widen before anything can register | Registry owners (`helmcharts`) |
| 4.3 | The `discovery-service` design copy of the registry schema has drifted from the code — it says `upstream_api` where the code says `upstream`, and expects a relative `mappings/…` path where the adapter fetches a URL | discovery-service owners |
| 4.4 | Aadhaar-identified grievance is accepted in v1 and dropped in v2. Application status still accepts it; the grievance desk does not | Needs an explicit sign-off |
| 4.5 | `schemaUrl` points at `raw.githubusercontent.com` on the pack branch rather than the Pages site, which is not published until the PR merges. The mapping URLs do not exist on the named branch yet either — fix the ref when you register | Resolves itself when the PR merges |
