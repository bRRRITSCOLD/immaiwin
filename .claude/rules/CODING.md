# Coding

## Standards
* DRY (Dont Repeat Yourself) - as best you can do not repeast yourself anf make things reusable. However, understand thata there is a fine line between DRY and Over Abstraction. Delicate Balance.

## UI
* Component library of choice shadcnui
* We always use @tanstack/react-form for forms with zod schema validation

## Environment Variables
* In code always use github.com/bRRRITSCOLD/enviro-go to load environment variables

## Audit Logging
* **Be vigilant.** Every time you add or modify a handler/endpoint that takes a meaningful action, **explicitly consider whether it should be audit-logged**. Don't ship the change without making the call.
* "Meaningful action" = anything that mutates state, grants/revokes access, exposes a secret/token, modifies billing/cost-bearing resources, changes membership, or has security/compliance implications. Pure reads are usually skippable; mutations almost never are.
* The decision *not* to audit is fine — but it must be a deliberate decision, not an oversight. If you skip, leave a one-line comment at the call site noting "no audit: <reason>" so future reviewers see the thought.
* Use `recordAudit(c, deps.Audit, action, target, metadata)` (authed paths) or `recordAuditUnauth(c, deps.Audit, action, actorEmail, userID, tenantID, metadata)` (login/reset/oauth-callback where ctx isn't yet stamped).
* Add new `AuditAction` constants in `internal/mongodb/audit_log.go` for new event types. Don't reshape existing values — dashboards/alerts depend on the strings.
* Audit writes are async + best-effort by design. They never block the user-facing response. A Mongo blip should not refuse a password change.
* When adding a new handler that needs audit, also add an option to the UI's `ACTION_FILTERS` in `internal/ui/src/routes/settings.tsx` so admins can filter on it.

