# Changelog

## [0.15.1](https://github.com/khuebt/herdr-runner-kit/compare/v0.15.0...v0.15.1) (2026-10-20)


### Bug Fixes

* wait for herdr to release a quit session's agent name before a relaunch ([#86](https://github.com/khuebt/herdr-runner-kit/issues/86)) ([da240ff](https://github.com/khuebt/herdr-runner-kit/commit/da240ff95d3e5e5012fafd715235f5ee6ee828b5))

## [0.15.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.14.0...v0.15.0) (2026-10-20)


### Features

* hold a Claude done for background subagents, shared-service rule, readable news lines ([#83](https://github.com/khuebt/herdr-runner-kit/issues/83)) ([f15515a](https://github.com/khuebt/herdr-runner-kit/commit/f15515ab677a01e0fdf140c2d423d0228d997e1d))


### Bug Fixes

* finish a held done after an interrupted turn, and only the done the watcher judged ([#85](https://github.com/khuebt/herdr-runner-kit/issues/85)) ([46f8b55](https://github.com/khuebt/herdr-runner-kit/commit/46f8b55e5d1278be9be74988e1e1948e007917b4))

## [0.14.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.13.0...v0.14.0) (2026-10-17)


### Features

* chain an independent read-only review runner for plan run and spawn ([#78](https://github.com/khuebt/herdr-runner-kit/issues/78)) ([2e7c331](https://github.com/khuebt/herdr-runner-kit/commit/2e7c331a2cb59a4d2f9d39bdcf570d186c356f0b))
* continue a claude runner once after a transient api error and report api-error news ([#77](https://github.com/khuebt/herdr-runner-kit/issues/77)) ([2e7c331](https://github.com/khuebt/herdr-runner-kit/commit/2e7c331a2cb59a4d2f9d39bdcf570d186c356f0b))


### Bug Fixes

* give wait --task its own cursor so it never moves the main session's ([#76](https://github.com/khuebt/herdr-runner-kit/issues/76)) ([2e7c331](https://github.com/khuebt/herdr-runner-kit/commit/2e7c331a2cb59a4d2f9d39bdcf570d186c356f0b))
* keep user text out of kit prompts in locked tabs and record a heavy command's pid before it runs ([#63](https://github.com/khuebt/herdr-runner-kit/issues/63)) ([2e7c331](https://github.com/khuebt/herdr-runner-kit/commit/2e7c331a2cb59a4d2f9d39bdcf570d186c356f0b))
* record when a runner started on the task instead of scanning the event log ([#62](https://github.com/khuebt/herdr-runner-kit/issues/62)) ([2e7c331](https://github.com/khuebt/herdr-runner-kit/commit/2e7c331a2cb59a4d2f9d39bdcf570d186c356f0b))

## [0.13.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.12.0...v0.13.0) (2026-10-17)


### ⚠ BREAKING CHANGES

* default kinds research and report to claude sonnet at medium effort ([#66](https://github.com/khuebt/herdr-runner-kit/issues/66))

### Features

* default kinds research and report to claude sonnet at medium effort ([#66](https://github.com/khuebt/herdr-runner-kit/issues/66)) ([f44f314](https://github.com/khuebt/herdr-runner-kit/commit/f44f3143c0c4a93104883bac536d5f9aef2896d8))
* record token usage per claude task and add hrk usage ([#69](https://github.com/khuebt/herdr-runner-kit/issues/69)) ([f44f314](https://github.com/khuebt/herdr-runner-kit/commit/f44f3143c0c4a93104883bac536d5f9aef2896d8))
* start claude runners without unused tools or mcp servers, and keep mcp with spawn --mcp ([#67](https://github.com/khuebt/herdr-runner-kit/issues/67)) ([f44f314](https://github.com/khuebt/herdr-runner-kit/commit/f44f3143c0c4a93104883bac536d5f9aef2896d8))
* wake the main session only for news that needs it and bring the rest with the next wake ([#68](https://github.com/khuebt/herdr-runner-kit/issues/68)) ([f44f314](https://github.com/khuebt/herdr-runner-kit/commit/f44f3143c0c4a93104883bac536d5f9aef2896d8))


### Bug Fixes

* count Workflow agents in Claude token usage ([8e8d91a](https://github.com/khuebt/herdr-runner-kit/commit/8e8d91a3c2d4b71075a9ddc67913310b16f39f8d))
* record a session's transcript when it starts ([8e8d91a](https://github.com/khuebt/herdr-runner-kit/commit/8e8d91a3c2d4b71075a9ddc67913310b16f39f8d))
* say on a kept worktree workspace that it is kept and how it goes away ([#65](https://github.com/khuebt/herdr-runner-kit/issues/65)) ([f44f314](https://github.com/khuebt/herdr-runner-kit/commit/f44f3143c0c4a93104883bac536d5f9aef2896d8))
* wake the main session for a reopened plan phase's done ([8e8d91a](https://github.com/khuebt/herdr-runner-kit/commit/8e8d91a3c2d4b71075a9ddc67913310b16f39f8d))


### Documentation

* say when quiet news waits and where the re-arm hook runs ([8e8d91a](https://github.com/khuebt/herdr-runner-kit/commit/8e8d91a3c2d4b71075a9ddc67913310b16f39f8d))
* tell the main agent to delegate lookups and never poll runners, and runners to use a light read-only agent ([#70](https://github.com/khuebt/herdr-runner-kit/issues/70)) ([f44f314](https://github.com/khuebt/herdr-runner-kit/commit/f44f3143c0c4a93104883bac536d5f9aef2896d8))

## [0.12.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.11.0...v0.12.0) (2026-10-16)


### ⚠ BREAKING CHANGES

* default kind code to claude sonnet at medium effort ([#56](https://github.com/khuebt/herdr-runner-kit/issues/56))

### Features

* default kind code to claude sonnet at medium effort ([#56](https://github.com/khuebt/herdr-runner-kit/issues/56)) ([f0183cd](https://github.com/khuebt/herdr-runner-kit/commit/f0183cd36d47c787779e19d49c34d8031c14e8d4))
* lock runner tabs so prompts come only from the kit ([#58](https://github.com/khuebt/herdr-runner-kit/issues/58)) ([f0183cd](https://github.com/khuebt/herdr-runner-kit/commit/f0183cd36d47c787779e19d49c34d8031c14e8d4))
* run a per-repository setup step in new runner worktrees and name the checks in done reports ([#57](https://github.com/khuebt/herdr-runner-kit/issues/57)) ([f0183cd](https://github.com/khuebt/herdr-runner-kit/commit/f0183cd36d47c787779e19d49c34d8031c14e8d4))
* share heavy command slots between runners with hrk heavy ([#55](https://github.com/khuebt/herdr-runner-kit/issues/55)) ([f0183cd](https://github.com/khuebt/herdr-runner-kit/commit/f0183cd36d47c787779e19d49c34d8031c14e8d4))
* warn in doctor when herdr's systemd unit has no oomd preference ([#53](https://github.com/khuebt/herdr-runner-kit/issues/53)) ([f0183cd](https://github.com/khuebt/herdr-runner-kit/commit/f0183cd36d47c787779e19d49c34d8031c14e8d4))


### Bug Fixes

* count a codex runner's done once its turn ends ([#54](https://github.com/khuebt/herdr-runner-kit/issues/54)) ([f0183cd](https://github.com/khuebt/herdr-runner-kit/commit/f0183cd36d47c787779e19d49c34d8031c14e8d4))
* retry the agent start when a new pane is busy and remove the empty worktree of a runner that never started ([#51](https://github.com/khuebt/herdr-runner-kit/issues/51)) ([f0183cd](https://github.com/khuebt/herdr-runner-kit/commit/f0183cd36d47c787779e19d49c34d8031c14e8d4))
* skip codex's update prompt and ask the user before trusting new hooks ([#52](https://github.com/khuebt/herdr-runner-kit/issues/52)) ([f0183cd](https://github.com/khuebt/herdr-runner-kit/commit/f0183cd36d47c787779e19d49c34d8031c14e8d4))

## [0.11.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.10.0...v0.11.0) (2026-10-16)


### Features

* run runners on pi and omp ([#39](https://github.com/khuebt/herdr-runner-kit/issues/39)) ([3a8bd61](https://github.com/khuebt/herdr-runner-kit/commit/3a8bd6140fafeaad943387a372aed3340e73f2ca))


### Bug Fixes

* close a runner's workspace once its worktree is gone ([#46](https://github.com/khuebt/herdr-runner-kit/issues/46)) ([4c42d7e](https://github.com/khuebt/herdr-runner-kit/commit/4c42d7e78c6579291a7de8dc815d9ec49f69e8ff))
* fail a runner whose agent exited back to its shell after a herdr restart ([#44](https://github.com/khuebt/herdr-runner-kit/issues/44)) ([4c42d7e](https://github.com/khuebt/herdr-runner-kit/commit/4c42d7e78c6579291a7de8dc815d9ec49f69e8ff))
* limit gc to the current main session and project runners by default ([#45](https://github.com/khuebt/herdr-runner-kit/issues/45)) ([4c42d7e](https://github.com/khuebt/herdr-runner-kit/commit/4c42d7e78c6579291a7de8dc815d9ec49f69e8ff))
* publish release candidates with notes ([#47](https://github.com/khuebt/herdr-runner-kit/issues/47)) ([4c42d7e](https://github.com/khuebt/herdr-runner-kit/commit/4c42d7e78c6579291a7de8dc815d9ec49f69e8ff))
* resubmit a first prompt whose Enter was lost ([#49](https://github.com/khuebt/herdr-runner-kit/issues/49)) ([4c42d7e](https://github.com/khuebt/herdr-runner-kit/commit/4c42d7e78c6579291a7de8dc815d9ec49f69e8ff))
* run each runner's agent in its own memory-capped systemd scope on Linux ([#43](https://github.com/khuebt/herdr-runner-kit/issues/43)) ([4c42d7e](https://github.com/khuebt/herdr-runner-kit/commit/4c42d7e78c6579291a7de8dc815d9ec49f69e8ff))
* show the shared plan-run worktree in show and list ([#48](https://github.com/khuebt/herdr-runner-kit/issues/48)) ([4c42d7e](https://github.com/khuebt/herdr-runner-kit/commit/4c42d7e78c6579291a7de8dc815d9ec49f69e8ff))
* wake the main session again after herdr restores it with claude --resume ([#42](https://github.com/khuebt/herdr-runner-kit/issues/42)) ([4c42d7e](https://github.com/khuebt/herdr-runner-kit/commit/4c42d7e78c6579291a7de8dc815d9ec49f69e8ff))

## [0.10.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.9.0...v0.10.0) (2026-10-15)


### Features

* shorten the command to hrk and keep herdr-runner-kit as an alias ([#37](https://github.com/khuebt/herdr-runner-kit/issues/37)) ([a14ef07](https://github.com/khuebt/herdr-runner-kit/commit/a14ef07ea91fbbcd24c8b0c94ca442345056d00e))

## [0.9.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.8.0...v0.9.0) (2026-10-15)


### ⚠ BREAKING CHANGES

* close kept runners on time, quieter stalls, squash-aware cleanup and compact show ([#32](https://github.com/khuebt/herdr-runner-kit/issues/32))

### Features

* close kept runners on time, quieter stalls, squash-aware cleanup and compact show ([#32](https://github.com/khuebt/herdr-runner-kit/issues/32)) ([841acaa](https://github.com/khuebt/herdr-runner-kit/commit/841acaa18b7d253ec61c80d6b6f2c8cb9ea3cff1))
* hand a runner near its context limit to a fresh session once ([#35](https://github.com/khuebt/herdr-runner-kit/issues/35)) ([99be511](https://github.com/khuebt/herdr-runner-kit/commit/99be511150a7735fa93fb9dc35c9d990f2bf428c))
* run an AgentKit plan one fresh runner per phase ([#34](https://github.com/khuebt/herdr-runner-kit/issues/34)) ([390bbed](https://github.com/khuebt/herdr-runner-kit/commit/390bbedd1a40def0923de70693541e0b2f333354))

## [0.8.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.7.2...v0.8.0) (2026-10-14)


### Features

* **project:** ask a direction round before full ux wireframes and design prototype ([#30](https://github.com/khuebt/herdr-runner-kit/issues/30)) ([9ac01c1](https://github.com/khuebt/herdr-runner-kit/commit/9ac01c195a1e60c4859f669991bc2733c79066db))

## [0.7.2](https://github.com/khuebt/herdr-runner-kit/compare/v0.7.1...v0.7.2) (2026-10-14)


### Bug Fixes

* **project:** keep local paths, credentials and feedback out of committed project files ([#28](https://github.com/khuebt/herdr-runner-kit/issues/28)) ([168b8a9](https://github.com/khuebt/herdr-runner-kit/commit/168b8a984c45c351bde4ad1893111d43896c88e6))

## [0.7.1](https://github.com/khuebt/herdr-runner-kit/compare/v0.7.0...v0.7.1) (2026-10-13)


### Bug Fixes

* remove the empty repository folder after the last worktree goes ([#26](https://github.com/khuebt/herdr-runner-kit/issues/26)) ([bae141b](https://github.com/khuebt/herdr-runner-kit/commit/bae141b213577cb5fa853c6343afca2daa249037))

## [0.7.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.6.0...v0.7.0) (2026-10-13)


### Features

* discovery stages, two hard gates and change control for projects ([#23](https://github.com/khuebt/herdr-runner-kit/issues/23)) ([428890e](https://github.com/khuebt/herdr-runner-kit/commit/428890e7063130f291c8bac5b12a452d40ff7d4f))
* milestone loop, release detection and graduation for projects ([#25](https://github.com/khuebt/herdr-runner-kit/issues/25)) ([57140c9](https://github.com/khuebt/herdr-runner-kit/commit/57140c998967f387df93e4b7161fbd12aa84cebf))

## [0.6.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.5.0...v0.6.0) (2026-10-13)


### Features

* start projects with new, a project record and status ([#21](https://github.com/khuebt/herdr-runner-kit/issues/21)) ([0272c1b](https://github.com/khuebt/herdr-runner-kit/commit/0272c1bc12bccb5f0d9744a43adf89f609bce39d))

## [0.5.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.4.0...v0.5.0) (2026-10-13)


### Features

* usage-limit fallback, branch sync and learning numbers ([#19](https://github.com/khuebt/herdr-runner-kit/issues/19)) ([edf9c1d](https://github.com/khuebt/herdr-runner-kit/commit/edf9c1d338866caacd9d051724c415dfb03b59c6))

## [0.4.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.3.0...v0.4.0) (2026-10-13)


### Features

* safer cleanup, git state in reports and runner keep ([#17](https://github.com/khuebt/herdr-runner-kit/issues/17)) ([860af5a](https://github.com/khuebt/herdr-runner-kit/commit/860af5a3e70c8310797f9f4e41603490ed51e717))

## [0.3.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.2.0...v0.3.0) (2026-10-13)


### Features

* brief before runners, named branches and worktree cleanup ([#15](https://github.com/khuebt/herdr-runner-kit/issues/15)) ([25b7349](https://github.com/khuebt/herdr-runner-kit/commit/25b73497dec4dd46f2bf581b9e128fd8f8ce1d4b))

## [0.2.0](https://github.com/khuebt/herdr-runner-kit/compare/v0.1.1...v0.2.0) (2026-10-13)


### Features

* pick a Claude config folder per runner ([#12](https://github.com/khuebt/herdr-runner-kit/issues/12)) ([e4261fa](https://github.com/khuebt/herdr-runner-kit/commit/e4261facb0d7c6df02ef56cd69949d21c5f6743d))

## [0.1.1](https://github.com/khuebt/herdr-runner-kit/compare/v0.1.0...v0.1.1) (2026-10-12)


### Bug Fixes

* release notes heading and closing runners in use ([#10](https://github.com/khuebt/herdr-runner-kit/issues/10)) ([dc456f7](https://github.com/khuebt/herdr-runner-kit/commit/dc456f715b227087b8fd6551ca068e948ceb4f8f))

## 0.1.0 (2026-10-12)


### Features

* add CLI skeleton with version command ([e3768b5](https://github.com/khuebt/herdr-runner-kit/commit/e3768b5b7a584e72a853f5322137134fc637e97b))
* add install script and upgrade ([#8](https://github.com/khuebt/herdr-runner-kit/issues/8)) ([6f77118](https://github.com/khuebt/herdr-runner-kit/commit/6f771186aa88b4975c20b60477de810ec9c74d81))
* add setup, doctor and the main-agent skill ([#7](https://github.com/khuebt/herdr-runner-kit/issues/7)) ([bf80301](https://github.com/khuebt/herdr-runner-kit/commit/bf80301faa2b6c41e3f87169221c265cb9d04c5b))
* add task model, config, state store and agent adapters ([#3](https://github.com/khuebt/herdr-runner-kit/issues/3)) ([281fb6f](https://github.com/khuebt/herdr-runner-kit/commit/281fb6f620d6f5c32fb63a41041880bba4b0cc6f))
* launch runners in herdr tabs ([#4](https://github.com/khuebt/herdr-runner-kit/issues/4)) ([bf419b4](https://github.com/khuebt/herdr-runner-kit/commit/bf419b4aab4051df3bba563d704c2603f478e936))
* report results, wait for news and chain tasks ([#5](https://github.com/khuebt/herdr-runner-kit/issues/5)) ([533868f](https://github.com/khuebt/herdr-runner-kit/commit/533868fd95a1ab6299fe2104f7371b171934a28b))
* send runner news to Telegram and route replies ([#6](https://github.com/khuebt/herdr-runner-kit/issues/6)) ([b96c36e](https://github.com/khuebt/herdr-runner-kit/commit/b96c36ebb95bc288edcdcde90b82a6e6744d6fae))


### Bug Fixes

* acceptance fixes for v0.1.0 ([#9](https://github.com/khuebt/herdr-runner-kit/issues/9)) ([3b1bc8f](https://github.com/khuebt/herdr-runner-kit/commit/3b1bc8fe7e1cea0fd47c6e42e821b5f4bdacc663))

## Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

This file is generated by release-please from Conventional Commit messages.
Do not edit it by hand.
