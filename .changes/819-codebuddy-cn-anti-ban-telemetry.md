### fix(codebuddy-cn): replicate official device identity and telemetry to prevent bans

Port of upstream `decolua/9router` PR #3257 (`feat(codebuddy-cn): anti-ban device
fingerprint + telemetry replication`), resolving issue #819.

Tencent cross-references device fingerprints sent in `/v2/report` telemetry. A
CodeBuddy CN key proxied through the gateway previously sent chat requests with
zero telemetry and an outdated User-Agent (`2.108.1`), which Tencent's ban
detector flagged as a silent/bot client and banned.

Changes applied:
- **Stable per-credential device identity** (`codebuddy_identity.go`): derives
  deterministic `qimei36` (36 hex), `machineId` (UUID), `sessionId` (UUID),
  `deviceId` (16 hex), `hostId` (32 hex), plus consistent VCS and hardware
  profiles from the credential seed. Stable across process restarts, unique per
  credential (no cross-contamination), and never leaks the raw secret.
- **Fail-open anti-ban telemetry** (`codebuddy_telemetry.go`): replicates the
  official CLI's lifecycle events (`ide_lifecycle`, `plugin_status`), pre-chat
  events (`agent_task_created`, `chat_message_send`, `chat_request_send`),
  post-chat events (`chat_message_response`, `agent_task_completed`), and
  Galileo OpenTelemetry traces. All sends are fire-and-forget on detached
  goroutines with a hard 5-second timeout — never delays, blocks, or alters the
  outcome of a chat request.
- **Header & registry refresh**:
  - `codebuddy-cn` User-Agent bumped `2.108.1` → `2.133.1` (matching official CLI).
  - Added `X-IDE-Version: 2.133.1` and `X-Domain: www.codebuddy.cn`.
  - `codebuddy-intl` (WorkBuddy) identity stays untouched and passes through
    with zero telemetry overhead.
