# Changelog

## [1.2.0](https://github.com/wichert/minicrm/compare/v1.1.0...v1.2.0) (2026-10-29)


### Features

* **inbox:** streamline queue actions, customer history, and mobile cards ([#17](https://github.com/wichert/minicrm/issues/17)) ([9cfa662](https://github.com/wichert/minicrm/commit/9cfa662e3c8cea4005a4ebd29025a5f313ef90f6))
* **ops:** add /healthz probe and wait for schema in seed CLI ([#20](https://github.com/wichert/minicrm/issues/20)) ([57f12be](https://github.com/wichert/minicrm/commit/57f12beffc34544a5c7fae3541027957441662d4))
* **seed:** load demo content from YAML through the real pipeline ([#16](https://github.com/wichert/minicrm/issues/16)) ([ff6971d](https://github.com/wichert/minicrm/commit/ff6971d19c666d94a48a8112c9d3e704a2a0c33e))
* **web:** compact inbox chrome, filters and rows (W2) ([#25](https://github.com/wichert/minicrm/issues/25)) ([1914e80](https://github.com/wichert/minicrm/commit/1914e804490ef42065d68e4601d4ed36ea59e3d9))
* **web:** compact mobile thread detail with reachable reply (W5) ([#28](https://github.com/wichert/minicrm/issues/28)) ([55fd60b](https://github.com/wichert/minicrm/commit/55fd60bbdfe6a1ed3bcdbf40cb0805485c788dbf))
* **web:** contextual bulk actions bar and mobile selection mode (W3) ([#27](https://github.com/wichert/minicrm/issues/27)) ([5307ef5](https://github.com/wichert/minicrm/commit/5307ef5ea6a820a49fceabfdaa91d4cbc419a8a7))
* **web:** inbox parity, sorting, pagination and keyboard navigation (W7) ([#31](https://github.com/wichert/minicrm/issues/31)) ([fa931f9](https://github.com/wichert/minicrm/commit/fa931f9765cfefbc00713561a7d511d296c978fb))
* **web:** mobile card layout and sectioned settings page (W6) ([#30](https://github.com/wichert/minicrm/issues/30)) ([e1841ae](https://github.com/wichert/minicrm/commit/e1841aeb5750d66985ed49779bfcc865bbcc2cd3))
* **web:** readable split pane, quiet list, Longest waiting sort, richer history panel ([#32](https://github.com/wichert/minicrm/issues/32)) ([d012841](https://github.com/wichert/minicrm/commit/d01284106bb6c8a4e2f1f50653e6750cff49b085))
* **web:** split master-detail layout on wide viewports (W4) ([#29](https://github.com/wichert/minicrm/issues/29)) ([71ba142](https://github.com/wichert/minicrm/commit/71ba14245633cc59af98a656f4522cdd6cea93b3))
* **web:** thread-age badges and Needs attention inbox view (W1) ([#24](https://github.com/wichert/minicrm/issues/24)) ([33110ec](https://github.com/wichert/minicrm/commit/33110ec8486590f2b27f3cd4b4b13069d48abf65))


### Bug Fixes

* **compose:** make mailpit wait for the dev TLS certificate ([#21](https://github.com/wichert/minicrm/issues/21)) ([153f1e4](https://github.com/wichert/minicrm/commit/153f1e4ce7dfa86fc91ce516c57c1f0c5e2ecd25))
* **seed:** supply unused required config variables in the seed CLI ([#19](https://github.com/wichert/minicrm/issues/19)) ([48381ef](https://github.com/wichert/minicrm/commit/48381effae4e3679841e20e332b792c58ec1a0d0))
* **web:** render form control text at a normal font weight ([#22](https://github.com/wichert/minicrm/issues/22)) ([669e2e1](https://github.com/wichert/minicrm/commit/669e2e1d502a770d2b5f329f73cce39dec9a0037))

## [1.1.0](https://github.com/wichert/minicrm/compare/v1.0.0...v1.1.0) (2026-10-28)


### Features

* **compose:** support formatted replies and drafts ([#10](https://github.com/wichert/minicrm/issues/10)) ([0a2ee76](https://github.com/wichert/minicrm/commit/0a2ee76af22a570b95ef11e6bdd232d2cae17d64))
* **drafts:** persist shared reply drafts ([#9](https://github.com/wichert/minicrm/issues/9)) ([0823ca5](https://github.com/wichert/minicrm/commit/0823ca592ebe79d498e0257bc7d0b95ef448db85))
* **inbox:** add bulk spam triage and recovery ([#11](https://github.com/wichert/minicrm/issues/11)) ([a06ab15](https://github.com/wichert/minicrm/commit/a06ab151258ed2b9881b4b8ed7d1e0c06493b54c))
* **inbox:** assign conversations from the queue ([#12](https://github.com/wichert/minicrm/issues/12)) ([9cfeecb](https://github.com/wichert/minicrm/commit/9cfeecbe297e6826b7b7d1a8b4323ec53bdee1a9))
* **inbox:** indicate mobile status-tab overflow ([#4](https://github.com/wichert/minicrm/issues/4)) ([ad72cb7](https://github.com/wichert/minicrm/commit/ad72cb73657e5f275b8d108846e959e76b9f2dc7))
* **inbox:** paginate and search conversations ([#5](https://github.com/wichert/minicrm/issues/5)) ([2d4aa05](https://github.com/wichert/minicrm/commit/2d4aa059d5bb1acfe1f20002fcf9f56f962367a8))
* **inbox:** show customer conversation history ([#6](https://github.com/wichert/minicrm/issues/6)) ([319d206](https://github.com/wichert/minicrm/commit/319d206c9f6a91ba08bd45ec0fc85aa5bbbcdee5))
* **mail:** allow per-message remote image loading ([#8](https://github.com/wichert/minicrm/issues/8)) ([27d6227](https://github.com/wichert/minicrm/commit/27d62271b2601e71df27b215523a28cae2cfaa7c))
* **workflow:** complete new conversations atomically ([#7](https://github.com/wichert/minicrm/issues/7)) ([8b772fc](https://github.com/wichert/minicrm/commit/8b772fc319ed39c739762b6b85a48a1445b1636b))

## 1.0.0 (2026-10-22)


### Features

* **ci:** automate releases with test-gated image publishing ([f78a554](https://github.com/wichert/minicrm/commit/f78a5543c644d3b3158dfc60d1b30a6acd577106))
* **web:** add progressive web app support and refresh-aligned sessions ([#1](https://github.com/wichert/minicrm/issues/1)) ([0f0e709](https://github.com/wichert/minicrm/commit/0f0e7092b1d1d932274d15c4b912a7a077525c50))


### Bug Fixes

* **release:** give release-please an access token ([#2](https://github.com/wichert/minicrm/issues/2)) ([2144283](https://github.com/wichert/minicrm/commit/2144283035e05d7b5553ef85acac5b4f426e5995))
