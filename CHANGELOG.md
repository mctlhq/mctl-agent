# Changelog

## [1.15.0](https://github.com/mctlhq/mctl-agent/compare/1.14.2...1.15.0) (2026-07-15)


### Features

* **skills:** add statefulset-replicas-mismatch YAML skill ([b8496c7](https://github.com/mctlhq/mctl-agent/commit/b8496c76d4e71698bb47d551684cbe206d6aca19))


### Bug Fixes

* **ci:** detect claude-review SDK failure the outcome field misses ([65c5e2a](https://github.com/mctlhq/mctl-agent/commit/65c5e2a7762ef7b476ccaf37c5e23c35c2b44a0e))
* **ci:** detect claude-review SDK failure the outcome field misses ([6617ce6](https://github.com/mctlhq/mctl-agent/commit/6617ce698a756058f6428e79194e25cb8d8cd18d))
* give Sonnet 5 diagnosis call headroom for thinking + JSON output ([94c55c9](https://github.com/mctlhq/mctl-agent/commit/94c55c92ee88ff3708562687e80cd56a19b9800a))
* restore quota_adjust match for service-less tenant quota alerts ([0041dfd](https://github.com/mctlhq/mctl-agent/commit/0041dfd836f3ff94ba5d189972ba4d504dd95969))
* scan for text content block instead of indexing block 0 ([1ca6057](https://github.com/mctlhq/mctl-agent/commit/1ca6057fad4806f2c7b8f276c10804abed4220d7))
* **skills:** address second review round on statefulset skill ([8109f9c](https://github.com/mctlhq/mctl-agent/commit/8109f9cdbc0e70428e47eabaae5bcdadcd23fe31))
* **skills:** match statefulset-replicas-mismatch against alert evidence, not just logs ([8f1022e](https://github.com/mctlhq/mctl-agent/commit/8f1022eabdb036f795257be9ce2de3774c54d941))
* stop force-closing real incidents as fake orphans ([fa85bed](https://github.com/mctlhq/mctl-agent/commit/fa85bedf51d977653c00a09a9e2a5a59c4d42faa))
* stop force-closing real incidents as fake orphans ([ea0bdb6](https://github.com/mctlhq/mctl-agent/commit/ea0bdb6596264c57e2ad9b8f22bc94402cf2432e))
