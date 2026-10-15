# Changelog

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
