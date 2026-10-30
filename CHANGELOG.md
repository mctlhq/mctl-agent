# Changelog

## [2.9.0](https://github.com/mctlhq/mctl-agent/compare/2.8.0...2.9.0) (2026-10-30)


### Features

* **scoutspike:** add the spike scaffold to drafts that omit it ([0559660](https://github.com/mctlhq/mctl-agent/commit/05596604c46c317edc462d31c66ac8359a3853fe))
* **scoutspike:** add the spike scaffold to drafts that omit it ([41e5f3d](https://github.com/mctlhq/mctl-agent/commit/41e5f3daec9d47f881d3bbe67bd499463cff3f98))

## [2.8.0](https://github.com/mctlhq/mctl-agent/compare/2.7.0...2.8.0) (2026-10-28)


### Features

* add spike issue form with intake labels ([f8c319a](https://github.com/mctlhq/mctl-agent/commit/f8c319aa298c2eac9dd6be85ef50874566945d38))
* add spike issue form with intake labels ([dc51287](https://github.com/mctlhq/mctl-agent/commit/dc5128785fd37026e61c28a58493d576ab0c45da))
* **agents:** issue-191-spike-can-github-copilot-server-to-serve ([a1eedf0](https://github.com/mctlhq/mctl-agent/commit/a1eedf08d3cbed69296e255ada995e19e252abe9))
* **agents:** issue-191-spike-can-github-copilot-server-to-serve ([d92126e](https://github.com/mctlhq/mctl-agent/commit/d92126e3acaf82b971349570f78239f24e60a381))


### Bug Fixes

* **api:** log every Telegram callback press ([c6037c7](https://github.com/mctlhq/mctl-agent/commit/c6037c7794ebf5bb3bb5fa3336fe2dd23ac7abeb))
* **api:** log every Telegram callback press ([0bfbb01](https://github.com/mctlhq/mctl-agent/commit/0bfbb013a11ea58428ace16490b1e10dd2604045))

## [2.7.0](https://github.com/mctlhq/mctl-agent/compare/2.6.0...2.7.0) (2026-10-26)


### Features

* **agents:** issue-185-feat-telegram-one-tap-create-spike-butto ([ca4d3b1](https://github.com/mctlhq/mctl-agent/commit/ca4d3b12d426125add3f7c66f6cdd20ea4d4f982))
* **agents:** issue-185-feat-telegram-one-tap-create-spike-butto ([ffaa798](https://github.com/mctlhq/mctl-agent/commit/ffaa79860bcec1e9e45896331e16b56f6f39257c))


### Bug Fixes

* **scoutspike:** match Python dedupe key and close race windows ([71e8f54](https://github.com/mctlhq/mctl-agent/commit/71e8f542fddfef6b93b9b2bba61514374aa8f119))
* **scoutspike:** match Python dedupe key and close race windows ([770c4ab](https://github.com/mctlhq/mctl-agent/commit/770c4abd96d94ded8071fd223b0660ea5e1c7c67))

## [2.6.0](https://github.com/mctlhq/mctl-agent/compare/2.5.2...2.6.0) (2026-10-25)


### Features

* **alerts:** stay quiet on unmatched alerts already paged to humans ([af9a32b](https://github.com/mctlhq/mctl-agent/commit/af9a32bab0f3acdc5ae61b74ea1fb3763164d4a0))
* **alerts:** stay quiet on unmatched alerts already paged to humans ([5582d51](https://github.com/mctlhq/mctl-agent/commit/5582d51fdac0c39ad81c8211e9515d160288e3e2))

## [2.5.2](https://github.com/mctlhq/mctl-agent/compare/2.5.1...2.5.2) (2026-10-25)


### Bug Fixes

* **notify:** keep a held header when its send fails ([de18ffc](https://github.com/mctlhq/mctl-agent/commit/de18ffc1b54f292ebcd0cf3d5840e5c38a209866))
* **notify:** one Telegram message per incident ([d56b430](https://github.com/mctlhq/mctl-agent/commit/d56b4308a0b36bc19f7a52923032385384f90369))
* **notify:** one Telegram message per incident ([98e5ae9](https://github.com/mctlhq/mctl-agent/commit/98e5ae980ce1b18e8b5bf49acfcc820ca534d08f))

## [2.5.1](https://github.com/mctlhq/mctl-agent/compare/2.5.0...2.5.1) (2026-10-23)


### Bug Fixes

* **deps:** bump Go to 1.26.10 for GO-2026-6809 ([bd20b96](https://github.com/mctlhq/mctl-agent/commit/bd20b96e1b28195f0626e971673ccb10b0d80c83))
* **deps:** bump Go to 1.26.10 for GO-2026-6809 ([dc29825](https://github.com/mctlhq/mctl-agent/commit/dc298253b6f106431b947ce21ec335e7c468ab38))

## [2.5.0](https://github.com/mctlhq/mctl-agent/compare/2.4.2...2.5.0) (2026-10-11)


### Features

* **observability:** correlate tickets and logs with their trace ([03e5a4e](https://github.com/mctlhq/mctl-agent/commit/03e5a4e83dd9744a15d3d80c4ab780cffe7fd656))
* **observability:** correlate tickets and logs with their trace ([a0f9862](https://github.com/mctlhq/mctl-agent/commit/a0f98623c754def415a84dddca1f9b640b8225f9))


### Bug Fixes

* **observability:** correlate the pipeline's failure-path log lines ([e302209](https://github.com/mctlhq/mctl-agent/commit/e3022096043d61bb485b13dd5caf5a5ed6ee3828))

## [2.4.2](https://github.com/mctlhq/mctl-agent/compare/2.4.1...2.4.2) (2026-10-10)


### Bug Fixes

* **monitor:** dedup on alert name, exempt critical from flap cooldown ([0a5c770](https://github.com/mctlhq/mctl-agent/commit/0a5c770fda68ee0a88ae603647c1a7d3031c498c))
* **monitor:** dedup on alert name, exempt critical from flap cooldown ([585e7ef](https://github.com/mctlhq/mctl-agent/commit/585e7efed507ca7ef2f97fe753b035a04724a904)), closes [#162](https://github.com/mctlhq/mctl-agent/issues/162)
* **monitor:** honour severity=critical label in flap cooldown ([e405d45](https://github.com/mctlhq/mctl-agent/commit/e405d45de288bda1f5a564ecab324bc58791d4b5))
* **monitor:** honour severity=critical label in flap cooldown ([87c9d2d](https://github.com/mctlhq/mctl-agent/commit/87c9d2d713d090e64ad8b32e571f70c74c43619e))
* **monitor:** key alert dedup on alert name, skip flap cooldown for critical ([2abb6be](https://github.com/mctlhq/mctl-agent/commit/2abb6be9ef03f25108a37116a26c439a2d1481af)), closes [#162](https://github.com/mctlhq/mctl-agent/issues/162)
* **monitor:** pass blank alert name when deduping GitHub workflow tickets ([9678fdd](https://github.com/mctlhq/mctl-agent/commit/9678fdd8dd6364da831ffc3f7d25d788846b761e)), closes [#162](https://github.com/mctlhq/mctl-agent/issues/162)
* **monitor:** pass blank alert name when deduping poller tickets ([7fbda48](https://github.com/mctlhq/mctl-agent/commit/7fbda48be36d715df014d58e720f0893c12b3a3e)), closes [#162](https://github.com/mctlhq/mctl-agent/issues/162)

### Removed

* **api:** remove the external-agent webhook subscription API ([#161](https://github.com/mctlhq/mctl-agent/pull/161)): `GET/POST /api/v1/webhooks`, `DELETE /api/v1/webhooks/{id}`, `POST /api/v1/tickets/{id}/external-claims`, `PATCH /api/v1/tickets/{id}/external-results`, the MCP tools `mctl_agent_list_webhooks`, `mctl_agent_register_webhook`, `mctl_agent_delete_webhook`, and the `WEBHOOK_ENABLED` / `WEBHOOK_DEFAULT_TTL` settings. This is a removal of public API, released as a patch because it was never switched on in production (`WEBHOOK_ENABLED` was never set) and its only client, mctl-openclaw, is archived. `WEBHOOK_CALLBACK_URL` (Telegram webhook base) is unchanged. The tables `webhook_endpoints`, `external_events`, `external_deliveries` and `external_claims` are left in place.

## [2.4.1](https://github.com/mctlhq/mctl-agent/compare/2.4.0...2.4.1) (2026-10-10)


### Bug Fixes

* **deps:** bump Go to 1.26.9 for net/http advisories ([d5be11b](https://github.com/mctlhq/mctl-agent/commit/d5be11ba3e5757210ab2e4c70e735918c64d2c9a))
* **deps:** bump golang.org/x/net to v0.60.0 for GO-2026-6617 ([ea21c9a](https://github.com/mctlhq/mctl-agent/commit/ea21c9a1b15a8c2d029830e66527235e0e94ce20))
* **deps:** bump x/net to v0.60.0 and Go to 1.26.9 ([5c18674](https://github.com/mctlhq/mctl-agent/commit/5c186749dedb3e55cb188dd784443b55f61a8968))

## [2.4.0](https://github.com/mctlhq/mctl-agent/compare/2.3.0...2.4.0) (2026-10-01)


### Features

* **telegram:** forward private /start to the bot-start bridge ([3c7890e](https://github.com/mctlhq/mctl-agent/commit/3c7890e4dfbc52dabd096e4a9f99589023fd4f68))
* **telegram:** forward private /start to the bot-start bridge ([e88f436](https://github.com/mctlhq/mctl-agent/commit/e88f4360cb0d8b8bdaf14c044fe87c5b90dfd73f))


### Bug Fixes

* **botstart:** cap in-flight forwards and count every outcome ([63c7b31](https://github.com/mctlhq/mctl-agent/commit/63c7b31ca5f28fc0b9771b3f8ead757de28d7c10))
* **botstart:** close the forwarder before draining; fail closed ([2194479](https://github.com/mctlhq/mctl-agent/commit/2194479cacb61becaf463e43bb7613fe6a822971))

## [2.3.0](https://github.com/mctlhq/mctl-agent/compare/2.2.0...2.3.0) (2026-09-30)


### Features

* **agents:** issue-143-follow-ups-from-142-service-name-canonic ([313902f](https://github.com/mctlhq/mctl-agent/commit/313902fd6d51289aff1bc30e146db2e8a574ff49))


### Bug Fixes

* **agents:** harden service-name canonicalisation and escalation messages ([6f329a5](https://github.com/mctlhq/mctl-agent/commit/6f329a5074f799346f9b085cf807c844d49728f2))
* commit is what makes release-please pick it up. ([96a12ec](https://github.com/mctlhq/mctl-agent/commit/96a12eca8891e76a5f61aa87d18b3a6363d8e781))
* **diagnosis:** release the claude-sonnet-5-5 switch ([76b9279](https://github.com/mctlhq/mctl-agent/commit/76b92794a0b2ba860cdbc07e7ce1beef8ea9e8c5))
* **diagnosis:** release the claude-sonnet-5-5 switch ([96a12ec](https://github.com/mctlhq/mctl-agent/commit/96a12eca8891e76a5f61aa87d18b3a6363d8e781))

## [2.2.0](https://github.com/mctlhq/mctl-agent/compare/2.1.0...2.2.0) (2026-09-29)


### Features

* **agents:** issue-141-oomkilled-and-other-pod-scoped-skills-re ([1a1271f](https://github.com/mctlhq/mctl-agent/commit/1a1271faa9dcf65888cfb39d6488d1f2ca292b87))
* **monitor:** classify ContainerOOMKilled as a resource-limit alert ([3f4549b](https://github.com/mctlhq/mctl-agent/commit/3f4549b9f1410074f2dbb6118d7c496119083158))
* **monitor:** classify ContainerOOMKilled as a resource-limit alert ([5171d37](https://github.com/mctlhq/mctl-agent/commit/5171d37f70bc5cae02181be9ee6179fe9475264b))
* **monitor:** classify NodeMemoryHeadroomLow as a resource-limit alert ([301599b](https://github.com/mctlhq/mctl-agent/commit/301599b8052d177fed7447924f53cadf0e9ffcdb))
* **monitor:** classify NodeMemoryHeadroomLow as a resource-limit alert ([ffedcbe](https://github.com/mctlhq/mctl-agent/commit/ffedcbe5315ee1199d697532955c36d1a3ed4a3f))
* **telemetry:** trace ticket processing over OTLP and count LLM tokens ([29bb65e](https://github.com/mctlhq/mctl-agent/commit/29bb65e507ef3880e306fc1edac4c8b2137076bc))
* **telemetry:** trace ticket processing over OTLP and count LLM tokens ([0c9e8f2](https://github.com/mctlhq/mctl-agent/commit/0c9e8f2285363fc15505b14fffeec5646a006b58))


### Bug Fixes

* **agents:** address P1/P2 codex findings on issue-141-oomkilled-and-other-pod-scoped-skills-re ([31b04d5](https://github.com/mctlhq/mctl-agent/commit/31b04d58ff4651ddf515edd8021dc6ee744c3995))
* **agents:** address P1/P2 codex findings on issue-141-oomkilled-and-other-pod-scoped-skills-re ([9a91630](https://github.com/mctlhq/mctl-agent/commit/9a91630b5dd9a31d5eb49a9b39dd399ee11abfe9))
* **agents:** address P1/P2 codex findings on issue-141-oomkilled-and-other-pod-scoped-skills-re ([fc7d01d](https://github.com/mctlhq/mctl-agent/commit/fc7d01d08c0d3103fc68a1ae4568a488d4a53600))
* **agents:** address P1/P2 codex findings on issue-141-oomkilled-and-other-pod-scoped-skills-re ([a5354d5](https://github.com/mctlhq/mctl-agent/commit/a5354d5663722dd32a1b7f17eda1d41a27185b34))
* **agents:** issue-141-oomkilled-and-other-pod-scoped-skills-re ([b9547e6](https://github.com/mctlhq/mctl-agent/commit/b9547e6f90f75e399e2dbcf058af5909eac6d742))
* **telemetry:** address review — request outcome, fix span semantics ([5052029](https://github.com/mctlhq/mctl-agent/commit/5052029c9284ecb731fe1046f81b25038aa58b80))
* **telemetry:** follow the OTLP protocol variable; survive a failed reload ([d8c9f83](https://github.com/mctlhq/mctl-agent/commit/d8c9f8349a7e55bc082a94bfcd0125f2a33dce64))

## [2.1.0](https://github.com/mctlhq/mctl-agent/compare/2.0.1...2.1.0) (2026-09-03)


### Features

* **agents:** issue-101-go-1-25-13-bump-dockerfile-go-mod-toolch ([5e6649b](https://github.com/mctlhq/mctl-agent/commit/5e6649bf505314358ea891b5afd627c741728b56))

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
