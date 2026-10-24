---
type: fix
area: quota
summary: Read schedules stored under the legacy split auto-ping key
---

The local dashboard writes the schedules half under
<provider>AutoPingSchedules instead of inside the canonical key. The
scheduler's settings source now folds that key in per tick, with the
canonical schedule winning per connection, so existing dashboards keep
working against the canonical scheduler.
