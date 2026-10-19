### Fix Settings redirect to Endpoint & Key in mobile and unauthenticated sessions

- Removed read-only `/api/settings/database/stats` from `IsAlwaysProtectedPath` in `internal/middleware/dashboard_auth.go`. When `requireLogin` is disabled (default local setup without master password), fresh mobile browser sessions without admin cookies were rejected with 401 on Settings mount, causing `handleUnauthorized` to bounce the session through `/login` into `Endpoint & Key`.
- Added `/api/settings/database/stats` to `handleUnauthorized` exemption list in `web/src/api/client.ts` so telemetry queries never trigger full-app auth redirects.
- Prevented off-screen mobile sidebar drawer from capturing pointer events when collapsed by applying `pointer-events-none lg:pointer-events-auto`.
- Safely guarded touch and click button detection in `Sidebar.svelte` navigation (`(e.button !== undefined && e.button !== 0)`).
- Switched settings links in `EndpointView.svelte` to SPA `navigate('settings')`.
- Added unit test in `internal/middleware/dashboard_auth_test.go` and verified end-to-end with mobile viewport browser testing.
