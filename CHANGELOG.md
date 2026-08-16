# Changelog

## [1.16.0](https://github.com/mctlhq/mctl-agent/compare/1.15.6...1.16.0) (2026-08-16)


### Features

* **pipeline:** add an escalated status so diagnosed tickets stop claiming to be analyzing ([1f6ce9f](https://github.com/mctlhq/mctl-agent/commit/1f6ce9faf5e40492c73faaa060deb12d210e0a9d))
* **pipeline:** add an escalated status so diagnosed tickets stop claiming to be analyzing ([4ab7e7d](https://github.com/mctlhq/mctl-agent/commit/4ab7e7d553196f7e32547dfee62aaed6d435c911))


### Bug Fixes

* **pipeline:** close the remaining analyzing leaks in handleHighConfidenceFix ([67c0cc9](https://github.com/mctlhq/mctl-agent/commit/67c0cc9f0aed2885cab12f617862cfbddf784b86))
* **pipeline:** emit fix_failed before escalating, not after ([8c4bb92](https://github.com/mctlhq/mctl-agent/commit/8c4bb92c11cf3353a993eb53fbdc4dfba8c8ca92))
* **pipeline:** order the mctl-api syncs instead of racing them ([6102e45](https://github.com/mctlhq/mctl-agent/commit/6102e45d9466115c648bc4f33a394187d3f02410))
* **pipeline:** order the mctl-api syncs instead of racing them ([937c7c1](https://github.com/mctlhq/mctl-agent/commit/937c7c1a43c7923f20dd3de66a0e90cd5b6648ea))
* **pipeline:** send mctl-api a ticket snapshot, and test escalate ([e04153e](https://github.com/mctlhq/mctl-agent/commit/e04153e764a7bd3bb7970dde6964f28c7f10f940))
* **pipeline:** set analyzing before publishing to mctl-api ([a661a9f](https://github.com/mctlhq/mctl-agent/commit/a661a9f71f17397cae64ae20a59b6a998c84861c))
* **pipeline:** set analyzing before publishing to mctl-api ([60a9089](https://github.com/mctlhq/mctl-agent/commit/60a9089902afc4fd5514b19c3b479724b9eac098))
* **poller:** include EscalatedAfter in the resolveStale guard ([106ef91](https://github.com/mctlhq/mctl-agent/commit/106ef91b2473e3767573bf2c17c18f2976a39f5d))

## [1.15.6](https://github.com/mctlhq/mctl-agent/compare/1.15.5...1.15.6) (2026-08-15)


### Bug Fixes

* **agent:** retry ticket store init and stop logging the DB password ([2c3485e](https://github.com/mctlhq/mctl-agent/commit/2c3485e2aa8559ccce73db6bb72ab4629a5456ca))

## [1.15.5](https://github.com/mctlhq/mctl-agent/compare/1.15.4...1.15.5) (2026-08-15)


### Bug Fixes

* API hardening backlog from [#65](https://github.com/mctlhq/mctl-agent/issues/65) (context, body limits, error sanitization) ([42091ef](https://github.com/mctlhq/mctl-agent/commit/42091eff340a2d9a7a0980245228095181c518ae))

## [1.15.4](https://github.com/mctlhq/mctl-agent/compare/1.15.3...1.15.4) (2026-08-15)


### Bug Fixes

* **fixer:** re-read the GitHub token per API call so rotation lands ([b8eafb9](https://github.com/mctlhq/mctl-agent/commit/b8eafb951de69f49c7d0bed872bb78fd6c552c00))
* **fixer:** re-read the GitHub token per API call so rotation lands ([3bae35e](https://github.com/mctlhq/mctl-agent/commit/3bae35e5be1ea2417ddbe045b2bf077fd5a7631b))
* **fixer:** scope the GitHub token to GitHub API hosts only ([d7dc2a1](https://github.com/mctlhq/mctl-agent/commit/d7dc2a1ec65df912d19a5293e392c0a381703323))

## [1.15.3](https://github.com/mctlhq/mctl-agent/compare/1.15.2...1.15.3) (2026-08-15)


### Bug Fixes

* **api:** also fail closed when a chat allowlist is set without a webhook secret ([a8ec6d9](https://github.com/mctlhq/mctl-agent/commit/a8ec6d9fc73c210453221ad75cd6b5a209f538b8))
* **api:** fail closed when webhook secret is set without a Telegram chat allowlist ([2afd615](https://github.com/mctlhq/mctl-agent/commit/2afd615524e0979cd7634889e351e60ec2bca93d))
* **api:** fail closed when webhook secret is set without a Telegram chat allowlist ([960c10d](https://github.com/mctlhq/mctl-agent/commit/960c10daacbc3122566dfd14c36185f4d6273390))

## [1.15.2](https://github.com/mctlhq/mctl-agent/compare/1.15.1...1.15.2) (2026-08-14)


### Bug Fixes

* **config:** stop discarding the mctl-api service token ([8f488c5](https://github.com/mctlhq/mctl-agent/commit/8f488c59939d72f9c69079a6a2a86330e533b639))
* **config:** stop discarding the mctl-api service token ([fda922a](https://github.com/mctlhq/mctl-agent/commit/fda922a28cd355fccd1e8ba4d87aa1551e84ad3a))

## [1.15.1](https://github.com/mctlhq/mctl-agent/compare/1.15.0...1.15.1) (2026-08-14)


### Bug Fixes

* authenticate inbound agent control-plane routes ([6207ee7](https://github.com/mctlhq/mctl-agent/commit/6207ee7a2391aa6ea81391625ae787f87d3d6775))
* authenticate inbound agent control-plane routes ([b903ce4](https://github.com/mctlhq/mctl-agent/commit/b903ce4da2386a5b8c1b148e5dbacb9b1ffb88bb))
* **ci:** bump claude-code-action pin to v1.0.189 ([9484d4e](https://github.com/mctlhq/mctl-agent/commit/9484d4efcd086564322c1781f0ccd0a32713aab7))
* **ci:** bump claude-code-action pin to v1.0.189 ([a835574](https://github.com/mctlhq/mctl-agent/commit/a8355740bb59d54832d89f1a577ed4a83ee4677a))
