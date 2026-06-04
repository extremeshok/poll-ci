# poll-ci
a dead-simple, single-container CI for GitHub. It watches a repo by **polling** (outbound only — no webhooks, no inbound ports), runs that repo's checks in Docker, and reports results as native **GitHub commit statuses**. One process, one container.
