# NodeApi: карта методов и доменов

Документ задачи T11.1a. Фасад десктопа — `pkg/api/node.NodeApi` (`api.go`). Здесь перечислены
все экспортированные методы, домен, к которому метод относится, бэкенд, которому метод
делегирует после переноса (пусто — метод ещё на фасаде), и то, дотягивается ли он до
сети v1 (swarm, транспорт, службы, доступ, DHT, relay, выход). Колонка `.node.*` — число
обращений к полю `NodeApi.node` в теле метода; `.node.Swarm` — из них к `node.Swarm`.

Домены задают бэкенды (`backends.go`): `netBackend`, `contactBackend`, `chatBackend`,
`groupBackend`, `fileBackend`, `callBackend`, `vpnBackend`. Кошелёк, профиль, пароль,
настройки, обновления и отчёты остаются локальными: это хранилище v1, а не сеть.

При `protocol: 2` выбранные сетевые бэкенды — `v2PendingBackend`: каждый метод с результатом
`error` возвращает `ErrV2NotBuilt` («v2 ещё не собран»); метод без `error` возвращает нулевое
значение. Методы доменов «сеть и доступ» и «контакты» с T11.1b, «чаты», «группы и каналы» и
«файлы» с T11.1c, «звонки и стримы» с T11.1d — делегаты своих бэкендов (`v1NetBackend`/
`v1ContactBackend`/`v1ChatBackend`/`v1GroupBackend`/`v1FileBackend`/`v1CallBackend` при
`protocol: 1`, `v2PendingBackend` при `protocol: 2`) и своей проверки режима не делают. Остальной
сетевой домен (VPN) до T11.1e по-прежнему сам проверяет режим (`NodeApi.v2Unavailable`) и после
переноса станет таким же делегатом.

Всего экспортированных методов: **237**. В их телах обращений `.node.*`: **282**, из них к `.node.Swarm`: **42**. По всему пакету `pkg/api/node` (329 методов `NodeApi`, включая неэкспортированные помощники; без `_test.go`): **438** обращений `.node.*`, из них **87** к `.node.Swarm` (с `_test.go` — 449 и 88; в задаче указаны 417/88 — расхождение в подсчёте, см. примечание 5).

Обозначения: «сеть v1» — да, если метод доходит до v1 swarm/транспорта/служб/доступа/
DHT/relay/выхода; нет — если работает только с локальным хранилищем. Кошелёк ходит в TON
по HTTP, но не через сеть v1, поэтому в колонке стоит «нет» (см. примечание под таблицей).
«Нет» не значит, что метод доступен при `protocol: 2`: он может принадлежать сетевому домену,
который выключен целиком, и тогда тоже отвечает `ErrV2NotBuilt`.

| Метод | Домен | Бэкенд | Сеть v1 | `.node.*` | `.node.Swarm` |
|---|---|---|---:|---:|---:|
| `ActiveGroupCall` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `CallStreamStats` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `CaptureRTPBudget` | звонки и стримы | v1CallBackend | нет | 0 | 0 |
| `EndGroupCall` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `EndRoomCall` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `GetCallManager` | звонки и стримы | v1CallBackend | да | 1 | 0 |
| `GetCallPath` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `GetCallRelays` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `GetCallRoster` | звонки и стримы | v1CallBackend | да | 3 | 0 |
| `GetCallSlots` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `InviteToCall` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `JoinCall` | звонки и стримы | v1CallBackend | да | 7 | 0 |
| `KeepGroupCallAlive` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `KeepRoomCallAlive` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `LeaveCall` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `LeaveRoomCall` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `ListStreamQualities` | звонки и стримы | v1CallBackend | нет | 0 | 0 |
| `PublishCaptureRTP` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `ReportCallMember` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `RequestRelayChange` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `SetCallBackdrop` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `SetCaptureEncoder` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `SetDisplayRate` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `SetUploadCapacity` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `SetVADState` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `SetupCallCallbacks` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `SetupCallMediaCallbacks` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `SetupStreamInfoCallback` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `StartGroupCall` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `StartRoomCall` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `StartScreenShare` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `StartStream` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `StopScreenShare` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `SubmitCallAnswer` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `SwitchCallRelay` | звонки и стримы | v1CallBackend | да | 0 | 0 |
| `UnwatchStream` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `UpdateStreamQuality` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `WatchStream` | звонки и стримы | v1CallBackend | да | 2 | 0 |
| `ChatAttachments` | чаты | v1ChatBackend | да | 0 | 0 |
| `ChatDelete` | чаты | v1ChatBackend | да | 0 | 0 |
| `ChatEdit` | чаты | v1ChatBackend | да | 0 | 0 |
| `ChatHistory` | чаты | v1ChatBackend | да | 0 | 0 |
| `ChatMarkRead` | чаты | v1ChatBackend | да | 0 | 0 |
| `ChatMessage` | чаты | v1ChatBackend | да | 0 | 0 |
| `ChatPin` | чаты | v1ChatBackend | да | 0 | 0 |
| `ChatPins` | чаты | v1ChatBackend | да | 0 | 0 |
| `ChatReact` | чаты | v1ChatBackend | да | 0 | 0 |
| `ChatRows` | чаты | v1ChatBackend | да | 1 | 0 |
| `ChatSend` | чаты | v1ChatBackend | да | 0 | 0 |
| `ChatThread` | чаты | v1ChatBackend | да | 0 | 0 |
| `Conversations` | чаты | v1ChatBackend | да | 0 | 0 |
| `GetMessages` | чаты | v1ChatBackend | да | 3 | 0 |
| `ListSavedMessages` | чаты | v1ChatBackend | да | 2 | 0 |
| `MessageHandler` | чаты | v1ChatBackend | да | 2 | 0 |
| `OnMessageState` | чаты | v1ChatBackend | да | 2 | 0 |
| `ResetEncryptionSession` | чаты | v1ChatBackend | да | 2 | 0 |
| `SaveMessage` | чаты | v1ChatBackend | да | 2 | 0 |
| `SendMessage` | чаты | v1ChatBackend | да | 2 | 0 |
| `SubscribeConversations` | чаты | v1ChatBackend | нет | 0 | 0 |
| `UnsaveMessage` | чаты | v1ChatBackend | да | 2 | 0 |
| `AcceptMessageRequest` | контакты | v1ContactBackend | да | 0 | 0 |
| `BanContact` | контакты | v1ContactBackend | да | 0 | 0 |
| `BlockPeer` | контакты | v1ContactBackend | да | 0 | 0 |
| `CanRing` | контакты | v1ContactBackend | да | 1 | 0 |
| `DeleteContact` | контакты | v1ContactBackend | да | 2 | 0 |
| `DismissMessageRequest` | контакты | v1ContactBackend | да | 0 | 0 |
| `GetBlockedPeers` | контакты | v1ContactBackend | да | 0 | 0 |
| `GetContacts` | контакты | v1ContactBackend | да | 2 | 0 |
| `GetMessageRequests` | контакты | v1ContactBackend | да | 0 | 0 |
| `NewContact` | контакты | v1ContactBackend | да | 2 | 0 |
| `SetContactProfile` | контакты | v1ContactBackend | да | 0 | 0 |
| `UnblockPeer` | контакты | v1ContactBackend | да | 0 | 0 |
| `AcceptFile` | файлы | v1FileBackend | да | 0 | 0 |
| `CancelFile` | файлы | v1FileBackend | да | 0 | 0 |
| `DeclineFile` | файлы | v1FileBackend | да | 0 | 0 |
| `DownloadDir` | файлы | v1FileBackend | нет | 1 | 0 |
| `FileTransferPath` | файлы | v1FileBackend | да | 0 | 0 |
| `GetFileTransfer` | файлы | v1FileBackend | да | 0 | 0 |
| `ListFileTransfers` | файлы | v1FileBackend | да | 0 | 0 |
| `OnFileTransfer` | файлы | v1FileBackend | да | 2 | 0 |
| `SendFile` | файлы | v1FileBackend | да | 1 | 0 |
| `SendGroupFile` | файлы | v1FileBackend | да | 0 | 0 |
| `AddGroupMember` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatCategoriesSetup` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatChannelMove` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatChannelPlace` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatChannelRemove` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatChannelSave` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatChannels` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatGroupPhoto` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatMainChannel` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatMainChannelSave` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatThreadTitle` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ChatThreads` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `CreateGroup` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `DeleteGroup` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GetGroupInvite` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GetGroupMessages` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupInviteContacts` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupInviteCreate` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupInviteRevoke` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupInvites` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupJoinDecide` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupJoinRequests` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupModerationLog` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupRoleRemove` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupRoleSave` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupRoles` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupSnapshotExport` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupSnapshotImport` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `GroupTimeout` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `InspectGroupInvite` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `JoinGroup` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `LeaveGroup` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ListGroupMembers` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `ListGroups` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `RemoveGroupMember` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `SendGroupMessage` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `SetGroupRole` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `SubscribeGroupEvents` | группы и каналы | v1GroupBackend | нет | 0 | 0 |
| `TransferGroupOwnership` | группы и каналы | v1GroupBackend | да | 0 | 0 |
| `AddBootstrapNode` | сеть и доступ | v1NetBackend | да | 6 | 2 |
| `AddRelay` | сеть и доступ | v1NetBackend | да | 1 | 0 |
| `CheckProxy` | сеть и доступ | v1NetBackend | да | 0 | 0 |
| `ConnectToPeer` | сеть и доступ | v1NetBackend | да | 8 | 4 |
| `CreatePass` | сеть и доступ | v1NetBackend | да | 0 | 0 |
| `DeletePass` | сеть и доступ | v1NetBackend | да | 0 | 0 |
| `ExtendPass` | сеть и доступ | v1NetBackend | да | 0 | 0 |
| `ForgetInvite` | сеть и доступ | v1NetBackend | да | 1 | 0 |
| `GetAccessStatus` | сеть и доступ | v1NetBackend | да | 6 | 1 |
| `GetAnonymity` | сеть и доступ | v1NetBackend | нет | 4 | 0 |
| `GetBootstrapNodes` | сеть и доступ | v1NetBackend | да | 3 | 2 |
| `GetGuardStatus` | сеть и доступ | v1NetBackend | да | 8 | 5 |
| `GetNetworkStats` | сеть и доступ | v1NetBackend | да | 3 | 3 |
| `GetPass` | сеть и доступ | v1NetBackend | да | 0 | 0 |
| `GetPeers` | сеть и доступ | v1NetBackend | да | 1 | 1 |
| `GetProxySettings` | сеть и доступ | v1NetBackend | нет | 4 | 0 |
| `GetRelaySettings` | сеть и доступ | v1NetBackend | нет | 2 | 0 |
| `GetRelayStatus` | сеть и доступ | v1NetBackend | да | 4 | 2 |
| `JoinInvite` | сеть и доступ | v1NetBackend | да | 3 | 1 |
| `ListPasses` | сеть и доступ | v1NetBackend | да | 0 | 0 |
| `MeasureUploadCapacity` | сеть и доступ | v1NetBackend | да | 4 | 2 |
| `OnPeerState` | сеть и доступ | v1NetBackend | да | 2 | 0 |
| `ParseInvite` | сеть и доступ | v1NetBackend | нет | 0 | 0 |
| `PeerDiscoveryHandler` | сеть и доступ | v1NetBackend | да | 1 | 0 |
| `PinGuard` | сеть и доступ | v1NetBackend | да | 7 | 2 |
| `RemoveBootstrapNode` | сеть и доступ | v1NetBackend | да | 3 | 0 |
| `RemoveJoinedPeer` | сеть и доступ | v1NetBackend | да | 1 | 0 |
| `RemovePassDevice` | сеть и доступ | v1NetBackend | да | 0 | 0 |
| `RevokePass` | сеть и доступ | v1NetBackend | да | 0 | 0 |
| `RotateAccessSecret` | сеть и доступ | v1NetBackend | да | 1 | 0 |
| `SetAccessMode` | сеть и доступ | v1NetBackend | да | 4 | 1 |
| `SetAnonymity` | сеть и доступ | v1NetBackend | да | 7 | 1 |
| `SetGuardMode` | сеть и доступ | v1NetBackend | да | 5 | 2 |
| `SetProxySettings` | сеть и доступ | v1NetBackend | да | 5 | 1 |
| `SetRelaySettings` | сеть и доступ | v1NetBackend | да | 4 | 0 |
| `StartLocalDiscovery` | сеть и доступ | v1NetBackend | да | 5 | 0 |
| `StartVPNVia` | сеть и доступ | v1NetBackend | да | 8 | 5 |
| `StopLocalDiscovery` | сеть и доступ | v1NetBackend | да | 1 | 0 |
| `UnpinGuard` | сеть и доступ | v1NetBackend | да | 7 | 2 |
| `ChangePassword` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `GetMyID` | профиль, пароль, настройки | — | нет | 2 | 0 |
| `GetMyPubKey` | профиль, пароль, настройки | — | нет | 2 | 0 |
| `GetPrivacy` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `GetProtocol` | профиль, пароль, настройки | — | нет | 2 | 0 |
| `GetSettings` | профиль, пароль, настройки | — | нет | 2 | 0 |
| `IsRegistered` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `Login` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `LoginRemembered` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `Recover` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `Register` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `RememberUnlock` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `RememberedUnlock` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `SetPrivacy` | профиль, пароль, настройки | — | нет | 3 | 0 |
| `SetProtocol` | профиль, пароль, настройки | — | нет | 3 | 0 |
| `SetSettings` | профиль, пароль, настройки | — | нет | 3 | 0 |
| `StartNode` | профиль, пароль, настройки | — | да | 4 | 0 |
| `StopNode` | профиль, пароль, настройки | — | да | 1 | 0 |
| `TriggerPanicButton` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `VerifyPassword` | профиль, пароль, настройки | — | нет | 1 | 0 |
| `ConnectionReport` | логи и отчёты | — | да | 9 | 3 |
| `CheckSites` | VPN, рынок и оплата выхода | — | да | 2 | 1 |
| `ClearVPNHistory` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `ConnectLastExit` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `ForgetExit` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `GetExitFreePeers` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `GetExitGatewayStatus` | VPN, рынок и оплата выхода | — | да | 3 | 0 |
| `GetExitPayment` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `GetProbeDomains` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `GetRelayMarket` | VPN, рынок и оплата выхода | — | да | 2 | 0 |
| `GetSavedExits` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `GetSharingStatus` | VPN, рынок и оплата выхода | — | да | 5 | 0 |
| `GetSiteLists` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `GetVPNAutoSwitch` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `GetVPNHistory` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `GetVPNStatus` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `KillSwitchOn` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `ListExitNodes` | VPN, рынок и оплата выхода | — | да | 1 | 1 |
| `PayExit` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `ProbeExitNode` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `RefreshExitNodes` | VPN, рынок и оплата выхода | — | да | 2 | 0 |
| `SetAutoPay` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `SetCustomSites` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `SetExitFavorite` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `SetExitFreePeers` | VPN, рынок и оплата выхода | — | да | 2 | 0 |
| `SetExitGatewayEnabled` | VPN, рынок и оплата выхода | — | да | 2 | 0 |
| `SetExitName` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `SetKillSwitch` | VPN, рынок и оплата выхода | — | нет | 2 | 0 |
| `SetPayAsset` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `SetProbeDomains` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `SetVPNAutoSwitch` | VPN, рынок и оплата выхода | — | нет | 2 | 0 |
| `SetWholeComputer` | VPN, рынок и оплата выхода | — | да | 5 | 0 |
| `StartSharing` | VPN, рынок и оплата выхода | — | да | 3 | 0 |
| `StartVPN` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `StartVPNAt` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `StopSharing` | VPN, рынок и оплата выхода | — | да | 1 | 0 |
| `StopVPN` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `StopVPNForSwitch` | VPN, рынок и оплата выхода | — | да | 0 | 0 |
| `WholeComputerSupported` | VPN, рынок и оплата выхода | — | нет | 0 | 0 |
| `ExportTonRecoveryPhrase` | кошелёк | — | нет | 1 | 0 |
| `GetOnRampURL` | кошелёк | — | нет | 2 | 0 |
| `GetSupport` | кошелёк | — | нет | 0 | 0 |
| `GetTonBalance` | кошелёк | — | нет | 0 | 0 |
| `GetTonHistory` | кошелёк | — | нет | 0 | 0 |
| `GetTonTopUpLink` | кошелёк | — | нет | 0 | 0 |
| `GetTonWallet` | кошелёк | — | нет | 0 | 0 |
| `GetVault` | кошелёк | — | нет | 0 | 0 |
| `GetVoucherEarnings` | кошелёк | — | нет | 1 | 0 |
| `OwnHTTPClient` | кошелёк | — | нет | 1 | 0 |
| `PayForTraffic` | кошелёк | — | нет | 0 | 0 |
| `SendTon` | кошелёк | — | нет | 0 | 0 |
| `SendUsdt` | кошелёк | — | нет | 0 | 0 |
| `SetSupportPercent` | кошелёк | — | нет | 0 | 0 |
| `VaultCancelWithdraw` | кошелёк | — | нет | 0 | 0 |
| `VaultDeposit` | кошелёк | — | нет | 0 | 0 |
| `VaultExecuteWithdraw` | кошелёк | — | нет | 0 | 0 |
| `VaultRequestWithdraw` | кошелёк | — | нет | 0 | 0 |

## Примечания

1. Методы `chat*`, `group*`, `file*` в этой ревизии сами не обращаются к `.node.*`: они идут
   через фасады (`convsFacade`, `groupsFacade`, `files`), которые держат `n.node`. Поэтому в
   колонке `.node.*` у них нули, но домен всё равно сетевой.
2. `ConnectionReport` (домен «логи и отчёты») читает состояние swarm, чтобы собрать отчёт,
   поэтому помечен как сетевой, хотя наружу отдаёт только текст.
3. Кошелёк (`ton*`, `vault*`, `support*`, `OwnHTTPClient`) и профиль (`Register`, `Login`,
   `Recover`, `VerifyPassword`, `ChangePassword`, `Remember*`, `GetSettings`, `SetSettings`,
   `GetMyID`, `GetMyPubKey`, `IsRegistered`, `TriggerPanicButton`) работают с хранилищем v1 и
   при `protocol: 2` продолжают работать. TON-запросы идут по HTTP, не через swarm v1.
   `StartNode`/`StopNode` — жизненный цикл, а не домен: они остаются на фасаде и при
   `protocol: 2` запускают режим без сети. В таблице они отнесены к профилю.
4. Обновлений в `NodeApi` нет: обновление десктопа живёт в `echofog-desktop-oss/app_update.go`.
5. `protocol` хранится в `ui_settings.json` профиля (`Protocol` в `persistedSettings`),
   по умолчанию 1. `ECHOFOG_PROTOCOL=2` задаёт значение по умолчанию для сборки/запуска, но
   сохранённое в профиле значение его перекрывает. `NodeApi.GetProtocol`/`SetProtocol` читают
   и сохраняют его; смена действует со следующего запуска.
6. Числа `.node.*` считаны по AST (селекторы `n.node.<поле>`), включая повторные обращения в
   одной строке. Сумма по экспортированным методам меньше общего числа по пакету: часть
   обращений стоит в неэкспортированных помощниках (329 методов всего).
7. Колонка «Бэкенд» — реализация, которой метод делегирует при `protocol: 1`: `v1NetBackend`
   (39 методов домена «сеть и доступ») и `v1ContactBackend` (12 методов домена «контакты»),
   перенесённые в T11.1b; `v1ChatBackend` (22), `v1GroupBackend` (39), `v1FileBackend` (10),
   перенесённые в T11.1c; `v1CallBackend` (38 методов домена «звонки и стримы»), перенесённый в
   T11.1d; `—` — метод пока остаётся на фасаде и проверяет режим сам. При `protocol: 2` на месте
   любого сетевого бэкенда стоит `v2PendingBackend`.
