# Changelog

## [2.0.1](https://github.com/mctlhq/mctl-agent/compare/2.0.0...2.0.1) (2026-08-29)


### Bug Fixes

* never auto-merge on an unpersisted transition; carry the diagnosis ([cfc0d75](https://github.com/mctlhq/mctl-agent/commit/cfc0d752b5d57a99b4de4b009a7a8b7cf8145f9b))
* **pipeline:** do not announce a transition the store refused ([fad74e9](https://github.com/mctlhq/mctl-agent/commit/fad74e95874ed6034268b1f7f3a43aef5f947566))
* **pipeline:** guard and detach the post-failure diagnosis writes too ([1163403](https://github.com/mctlhq/mctl-agent/commit/1163403cb185994ee04c7ac009f2c4ff524ff27e))

## [2.0.0](https://github.com/mctlhq/mctl-agent/compare/1.17.1...2.0.0) (2026-08-29)


### ⚠ BREAKING CHANGES

* **agents:** remote skills previously registered with an http:// endpoint or a private/loopback/CGNAT-range host will fail outbound calls once this ships (the dial guard applies to already-registered skills, not just new registrations). Re-register them over https:// with a public/allowed host.

### Features

* **agents:** issue-99-path-allowlist-for-gitops-writes-remote ([01adb82](https://github.com/mctlhq/mctl-agent/commit/01adb82497e77dfb9502a7b244cd876f14ff8c3c))


### Bug Fixes

* **remote:** close DNS-rebinding TOCTOU in dial guard; bound registration DNS lookup ([c9d8a4b](https://github.com/mctlhq/mctl-agent/commit/c9d8a4b8b8c0c05297a1f8b449915acdbbc3195b))

## [1.17.1](https://github.com/mctlhq/mctl-agent/compare/1.17.0...1.17.1) (2026-08-29)


### Bug Fixes

* **agents:** issue-100-default-auto-merge-enabled-false-honor-c ([ac00a8f](https://github.com/mctlhq/mctl-agent/commit/ac00a8f59d8161911396528c6501bb2bd00be8ac))
* **monitor:** refuse to resolve tickets from a fingerprintless alert ([a34b538](https://github.com/mctlhq/mctl-agent/commit/a34b5382491887b6fb5e514e8a9387864a9fbdd9)), closes [#107](https://github.com/mctlhq/mctl-agent/issues/107)
* **ticket:** forward the context in ListAll; detach writes at the write ([0fe422c](https://github.com/mctlhq/mctl-agent/commit/0fe422c51904a07e5c845e49d4f89103fbefa119))

## [1.17.0](https://github.com/mctlhq/mctl-agent/compare/1.16.2...1.17.0) (2026-08-29)


### Features

* **agents:** issue-98-fail-closed-when-inbound-auth-tokens-are ([89a0826](https://github.com/mctlhq/mctl-agent/commit/89a082699156ead6f101853b481ffc6c8586189d))


### Bug Fixes

* **auth:** fail closed when inbound auth tokens are empty ([e106b53](https://github.com/mctlhq/mctl-agent/commit/e106b53bdc81e76816d1d1a1d533e1e2406acfe2))
* **monitor:** bound a replayed resolve to the alert's own occurrence ([40cb438](https://github.com/mctlhq/mctl-agent/commit/40cb438dfb7a4ad5a47833ad30e57323c8bf7648))
* **monitor:** carry the pre-rollout key through the workload-label switch ([8a162a1](https://github.com/mctlhq/mctl-agent/commit/8a162a138c4fffc35d5a3b74ab2b861904ea65b9))
* **monitor:** drop the key migration, let AM reconcile own the rollout window ([b7c9eba](https://github.com/mctlhq/mctl-agent/commit/b7c9ebad06521a326547016ca921a06856a4cea1))
* **monitor:** file workload alerts under the object, not kube-state-metrics ([7c06554](https://github.com/mctlhq/mctl-agent/commit/7c06554ab64f0d5ba392ae713d4d4dc2e7a338cf))
* **monitor:** keep legacy fingerprintless tickets resolvable; pin batch drain ([7b391ba](https://github.com/mctlhq/mctl-agent/commit/7b391ba004f335b4c8f39c5c2fbec90dc34ad3d1))
* **monitor:** make a replayed alert batch safe to apply twice ([b13c39e](https://github.com/mctlhq/mctl-agent/commit/b13c39ec4325afe217d07b106b78d7505ef02326))
* **monitor:** return 5xx so AlertManager retries a failed alert batch ([4d01593](https://github.com/mctlhq/mctl-agent/commit/4d01593cfe6123203ba2bf9ef97fd5cf5ff7563e))
* **monitor:** scope the pre-rollout key by fingerprint, not by name alone ([5145e9f](https://github.com/mctlhq/mctl-agent/commit/5145e9f23327e59a462c8fd15453e7c225d62c11))
* **monitor:** suppress flaps across the rollout boundary too ([2061a90](https://github.com/mctlhq/mctl-agent/commit/2061a90226c4627ddbacf4cf1fcf0c44736d68c9))

## [1.16.2](https://github.com/mctlhq/mctl-agent/compare/1.16.1...1.16.2) (2026-08-23)


### Bug Fixes

* **fixer:** handle comments in probe blocks and pair summaries correctly ([923a97a](https://github.com/mctlhq/mctl-agent/commit/923a97a3ac6c2fe949e177f48cc0535ea1b16f17))
* **fixer:** make probe_fix able to patch the files it is pointed at ([ad70909](https://github.com/mctlhq/mctl-agent/commit/ad709099d92d86b3539acb0b9e4139d6db03038b))
* **probe_fix:** only match the kubelet's own probe-failure phrasing ([c09c146](https://github.com/mctlhq/mctl-agent/commit/c09c14619bb22d8d163fdb83d2cab85b62bc289c))

## [1.16.1](https://github.com/mctlhq/mctl-agent/compare/1.16.0...1.16.1) (2026-08-18)


### Bug Fixes

* **api:** drop chi middleware.RealIP, which had no consumer ([39e1f2d](https://github.com/mctlhq/mctl-agent/commit/39e1f2d285a8c8b0eee2d3ce58b729eafed94457))
* **api:** drop chi middleware.RealIP, which had no consumer ([9b76bbd](https://github.com/mctlhq/mctl-agent/commit/9b76bbd247223011d02dd24112ee031aac391c52))
* **pipeline:** bound diagnosis, stop sharing the ticket pointer, unblock callers ([217bb64](https://github.com/mctlhq/mctl-agent/commit/217bb643bdcff028ad28fe70644515586cad361a))
* **pipeline:** bound diagnosis, stop sharing the ticket pointer, unblock callers ([2ee06a1](https://github.com/mctlhq/mctl-agent/commit/2ee06a13e784962321a50e3211b08905f439f1c7))

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
