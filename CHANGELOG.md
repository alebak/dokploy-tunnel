# Changelog

## [0.2.0](https://github.com/alebak/dokploy-tunnel/compare/v0.1.0...v0.2.0) (2026-10-07)


### Features

* **addressing:** short hostnames from the Dokploy appName ([#82](https://github.com/alebak/dokploy-tunnel/issues/82)) ([061d0ea](https://github.com/alebak/dokploy-tunnel/commit/061d0ea917ba00215b82c9ec83353f47cea901be))
* **cli:** add the tunnel client that forwards local connections to the companion ([#76](https://github.com/alebak/dokploy-tunnel/issues/76)) ([e82106d](https://github.com/alebak/dokploy-tunnel/commit/e82106ddd23e1b4a68a08a82c50452b07401760e))
* **cli:** forward services through the companion with doktunnel forward ([#79](https://github.com/alebak/dokploy-tunnel/issues/79)) ([16fd7b6](https://github.com/alebak/dokploy-tunnel/commit/16fd7b6db11eb71e6a4aaa9278477f6d52dff4c9))
* **cli:** report active forwards with doktunnel status ([#80](https://github.com/alebak/dokploy-tunnel/issues/80)) ([031bc8e](https://github.com/alebak/dokploy-tunnel/commit/031bc8ec0864cae3f6899f8ca7f9864ed93e2e6e))
* **companion:** report a target's exposed ports at /v1/ports ([#75](https://github.com/alebak/dokploy-tunnel/issues/75)) ([49f4fb8](https://github.com/alebak/dokploy-tunnel/commit/49f4fb80a9b83d24571a0554b23207ed3f806fe3))


### Bug Fixes

* **devcontainer:** install the pinned tools into GOPATH/bin ([#78](https://github.com/alebak/dokploy-tunnel/issues/78)) ([f0c5bfa](https://github.com/alebak/dokploy-tunnel/commit/f0c5bfa3baa1c72e45bbf104960587f22ca3a85b))

## 0.1.0 (2026-10-06)


### Features

* **addressing:** add stable loopback IP registry ([#28](https://github.com/alebak/dokploy-tunnel/issues/28)) ([ae07a86](https://github.com/alebak/dokploy-tunnel/commit/ae07a86fa7423ef9d6cc116de1f9f4c70ad2d585))
* **addressing:** derive DNS-safe .internal hostnames for leases ([#49](https://github.com/alebak/dokploy-tunnel/issues/49)) ([0f863ef](https://github.com/alebak/dokploy-tunnel/commit/0f863ef3cb3448e1bb579874aa66c7835cbc3632))
* **addressing:** manage a marked doktunnel section in the hosts file ([#50](https://github.com/alebak/dokploy-tunnel/issues/50)) ([2081eec](https://github.com/alebak/dokploy-tunnel/commit/2081eec4c67f869cf788fee0388c76fdd821048a))
* **api:** discover projects, environments and services with project.all ([#33](https://github.com/alebak/dokploy-tunnel/issues/33)) ([40d289e](https://github.com/alebak/dokploy-tunnel/commit/40d289e2991f675860e31c7f719adbd2f319238f))
* **api:** list the services inside a compose stack ([#40](https://github.com/alebak/dokploy-tunnel/issues/40)) ([2a8e39e](https://github.com/alebak/dokploy-tunnel/commit/2a8e39eb3e594bdaed149a31bc079c71052ff320))
* **api:** read service details for forwarding ([#34](https://github.com/alebak/dokploy-tunnel/issues/34)) ([7574ab9](https://github.com/alebak/dokploy-tunnel/commit/7574ab9ef365172839c35e6ec8df338b354df442))
* **auth:** add context commands to register Dokploy panels ([#32](https://github.com/alebak/dokploy-tunnel/issues/32)) ([1b99f3c](https://github.com/alebak/dokploy-tunnel/commit/1b99f3c179d8b190aca4507ab62c2e6194b7fbc5))
* **auth:** store contexts in a config file and API keys in the OS keyring ([#30](https://github.com/alebak/dokploy-tunnel/issues/30)) ([e0d75dd](https://github.com/alebak/dokploy-tunnel/commit/e0d75dd811233022cf4091dce4b886cb3a63b130))
* **auth:** validate Dokploy API keys and discover their organization ([#31](https://github.com/alebak/dokploy-tunnel/issues/31)) ([7c9df25](https://github.com/alebak/dokploy-tunnel/commit/7c9df2536b6f178de5469de198d0d9317bc90041))
* **cli:** add --skill to print an agent SKILL.md generated from the command tree ([#27](https://github.com/alebak/dokploy-tunnel/issues/27)) ([eea5bf3](https://github.com/alebak/dokploy-tunnel/commit/eea5bf35470a8980cfb44508f581a048d758ef2c))
* **cli:** add command tree with global flags ([#26](https://github.com/alebak/dokploy-tunnel/issues/26)) ([701bd52](https://github.com/alebak/dokploy-tunnel/commit/701bd5217e1a217323657e17a1f2f610063c4bba))
* **cli:** add hosts sync, list and clean with a privileged helper ([#51](https://github.com/alebak/dokploy-tunnel/issues/51)) ([a35f02e](https://github.com/alebak/dokploy-tunnel/commit/a35f02e3d59400e5242883cfcab2acb130340743))
* **cli:** add input policy for missing values ([#25](https://github.com/alebak/dokploy-tunnel/issues/25)) ([25ccf6d](https://github.com/alebak/dokploy-tunnel/commit/25ccf6d0141156e5350b7ad05af984946555f72b))
* **cli:** add stable error codes, exit codes and error rendering ([#24](https://github.com/alebak/dokploy-tunnel/issues/24)) ([1b2f434](https://github.com/alebak/dokploy-tunnel/commit/1b2f4347535a0fdb33b0f3fa3d3b2c713d8df2eb))
* **cli:** add the not_found error code ([#35](https://github.com/alebak/dokploy-tunnel/issues/35)) ([bb8a256](https://github.com/alebak/dokploy-tunnel/commit/bb8a256b71440215f068a0d400e8be0e4a894269))
* **cli:** complete database names and statuses in services ([#37](https://github.com/alebak/dokploy-tunnel/issues/37)) ([0dbb3e5](https://github.com/alebak/dokploy-tunnel/commit/0dbb3e5399ec072b4f81f401c021d743c39341c8))
* **cli:** discover and store each context's companion URL ([#70](https://github.com/alebak/dokploy-tunnel/issues/70)) ([2550120](https://github.com/alebak/dokploy-tunnel/commit/2550120d1bfd223be22ad2d5a95da1c87264f4fa))
* **cli:** let commands declare their own flags and arguments ([#29](https://github.com/alebak/dokploy-tunnel/issues/29)) ([49e297e](https://github.com/alebak/dokploy-tunnel/commit/49e297e5ac71a34c4c86f412d3328ec43f91d3ac))
* **cli:** list projects and services with doktunnel services ([#36](https://github.com/alebak/dokploy-tunnel/issues/36)) ([5253eca](https://github.com/alebak/dokploy-tunnel/commit/5253ecabf812abda4928c2de5b869e81ae6fa5e2))
* **cli:** list the services inside compose stacks ([#41](https://github.com/alebak/dokploy-tunnel/issues/41)) ([52a7827](https://github.com/alebak/dokploy-tunnel/commit/52a78278aacbef46384521b713b1fd52d4044e5b))
* **companion:** add a least-privilege Docker socket proxy ([#71](https://github.com/alebak/dokploy-tunnel/issues/71)) ([b6ecf06](https://github.com/alebak/dokploy-tunnel/commit/b6ecf063a42e5828251fd6daf34b27f07e0b09d7))
* **companion:** add a minimal Docker Engine API client ([#56](https://github.com/alebak/dokploy-tunnel/issues/56)) ([c6a9c77](https://github.com/alebak/dokploy-tunnel/commit/c6a9c77c535dfe50908ad08afb31feed51387fad))
* **companion:** authorize tunnel targets through the Dokploy API ([#44](https://github.com/alebak/dokploy-tunnel/issues/44)) ([4acf500](https://github.com/alebak/dokploy-tunnel/commit/4acf500daad2f46e801eba938808a05e75b55b28))
* **companion:** forward tunnels through socat repeaters by default ([#58](https://github.com/alebak/dokploy-tunnel/issues/58)) ([61935df](https://github.com/alebak/dokploy-tunnel/commit/61935df716220a011dc4a526e1a56f1bce2cb7e5))
* **companion:** handle tunnel WebSockets and hand them to a bridge ([#45](https://github.com/alebak/dokploy-tunnel/issues/45)) ([b46cf63](https://github.com/alebak/dokploy-tunnel/commit/b46cf6318145c33917166e867e280647a2a962bd))
* **companion:** resolve targets and manage shared repeater containers ([#57](https://github.com/alebak/dokploy-tunnel/issues/57)) ([64779c3](https://github.com/alebak/dokploy-tunnel/commit/64779c3b2bc82c36025251512959b28204b1b7d2))
* **companion:** run doktunnel-companion as a tunnel server ([#46](https://github.com/alebak/dokploy-tunnel/issues/46)) ([247d560](https://github.com/alebak/dokploy-tunnel/commit/247d5607af65619256d34e76b8357c5e738b69e8))


### Bug Fixes

* **addressing:** harden the privileged hosts helper ([#52](https://github.com/alebak/dokploy-tunnel/issues/52)) ([a2532f8](https://github.com/alebak/dokploy-tunnel/commit/a2532f88518a9ac43aa9f69e7d165e5b3ba4fdc0))
* **companion:** bind repeater dials to the target container, not only its IP ([#67](https://github.com/alebak/dokploy-tunnel/issues/67)) ([618c627](https://github.com/alebak/dokploy-tunnel/commit/618c627ebc69ae595130bf77e605891a215623e9))
* **companion:** close the remaining unsafe-target gaps in repeater resolution ([#65](https://github.com/alebak/dokploy-tunnel/issues/65)) ([5d05a56](https://github.com/alebak/dokploy-tunnel/commit/5d05a567ce362c1ec63de418cb27c3e25afaafdb))
* **companion:** judge the real Docker data root and weakened security profiles ([#69](https://github.com/alebak/dokploy-tunnel/issues/69)) ([5f3a063](https://github.com/alebak/dokploy-tunnel/commit/5f3a063b45dabfae400591563f8bd881b82f2c3e))
* **companion:** reap only repeater containers the companion created ([#66](https://github.com/alebak/dokploy-tunnel/issues/66)) ([8755b71](https://github.com/alebak/dokploy-tunnel/commit/8755b718636112acaf30cfa27c468a9a9a6c905c))
* **dokploy:** size response limits per procedure ([#48](https://github.com/alebak/dokploy-tunnel/issues/48)) ([75f3e55](https://github.com/alebak/dokploy-tunnel/commit/75f3e55f669b73e370210b1128e672d815dafadc))


### Code Refactoring

* **fsutil:** share one atomic file write between config and registry ([#47](https://github.com/alebak/dokploy-tunnel/issues/47)) ([01f8e44](https://github.com/alebak/dokploy-tunnel/commit/01f8e448c592d93415f58522bfb45ecba584b342))
