# NodeApi: карта методов и доменов

Документ задачи T11.1a. Фасад десктопа — `pkg/api/node.NodeApi` (`api.go`). Здесь перечислены
все экспортированные методы, домен, к которому метод относится, и то, дотягивается ли он до
сети v1 (swarm, транспорт, службы, доступ, DHT, relay, выход). Колонка `.node.*` — число
обращений к полю `NodeApi.node` в теле метода; `.node.Swarm` — из них к `node.Swarm`.

Домены задают бэкенды (`backends.go`): `netBackend`, `contactBackend`, `chatBackend`,
`groupBackend`, `fileBackend`, `callBackend`, `vpnBackend`. Кошелёк, профиль, пароль,
настройки, обновления и отчёты остаются локальными: это хранилище v1, а не сеть.

При `protocol: 2` выбранные сетевые бэкенды — `v2PendingBackend`: каждый метод с результатом
`error` возвращает `ErrV2NotBuilt` («v2 ещё не собран»); метод без `error` возвращает нулевое
значение. До T11.1b-e сами методы фасада проверяют режим (`NodeApi.v2Unavailable`) и ведут себя
так же; после T11.1b-e они станут делегатами этих интерфейсов.

Всего экспортированных методов: **237**. В их телах обращений `.node.*`: **282**, из них к `.node.Swarm`: **42**. По всему пакету `pkg/api/node` (326 методов, включая неэкспортированные помощники): **437** обращений `.node.*`, из них **87** к `.node.Swarm` (AST-подсчёт на этой ревизии; в задаче указаны 417/88 — расхождение в подсчёте, см. примечание 5).

Обозначения: «сеть v1» — да, если метод доходит до v1 swarm/транспорта/служб/доступа/
DHT/relay/выхода; нет — если работает только с локальным хранилищем. Кошелёк ходит в TON
по HTTP, но не через сеть v1, поэтому в колонке стоит «нет» (см. примечание под таблицей).

| Метод | Домен | Сеть v1 | `.node.*` | `.node.Swarm` |
|---|---|---:|---:|---:|
| `ActiveGroupCall` | звонки и стримы | да | 0 | 0 |
| `CallStreamStats` | звонки и стримы | да | 2 | 0 |
| `CaptureRTPBudget` | звонки и стримы | да | 0 | 0 |
| `EndGroupCall` | звонки и стримы | да | 0 | 0 |
| `EndRoomCall` | звонки и стримы | да | 0 | 0 |
| `GetCallManager` | звонки и стримы | да | 1 | 0 |
| `GetCallPath` | звонки и стримы | да | 0 | 0 |
| `GetCallRelays` | звонки и стримы | да | 0 | 0 |
| `GetCallRoster` | звонки и стримы | да | 3 | 0 |
| `GetCallSlots` | звонки и стримы | да | 2 | 0 |
| `InviteToCall` | звонки и стримы | да | 2 | 0 |
| `JoinCall` | звонки и стримы | да | 7 | 0 |
| `KeepGroupCallAlive` | звонки и стримы | да | 0 | 0 |
| `KeepRoomCallAlive` | звонки и стримы | да | 0 | 0 |
| `LeaveCall` | звонки и стримы | да | 2 | 0 |
| `LeaveRoomCall` | звонки и стримы | да | 0 | 0 |
| `ListStreamQualities` | звонки и стримы | да | 0 | 0 |
| `PublishCaptureRTP` | звонки и стримы | да | 2 | 0 |
| `ReportCallMember` | звонки и стримы | да | 2 | 0 |
| `RequestRelayChange` | звонки и стримы | да | 2 | 0 |
| `SetCallBackdrop` | звонки и стримы | да | 2 | 0 |
| `SetCaptureEncoder` | звонки и стримы | да | 2 | 0 |
| `SetDisplayRate` | звонки и стримы | да | 2 | 0 |
| `SetUploadCapacity` | звонки и стримы | да | 2 | 0 |
| `SetVADState` | звонки и стримы | да | 2 | 0 |
| `SetupCallCallbacks` | звонки и стримы | да | 2 | 0 |
| `SetupCallMediaCallbacks` | звонки и стримы | да | 2 | 0 |
| `SetupStreamInfoCallback` | звонки и стримы | да | 2 | 0 |
| `StartGroupCall` | звонки и стримы | да | 0 | 0 |
| `StartRoomCall` | звонки и стримы | да | 0 | 0 |
| `StartScreenShare` | звонки и стримы | да | 2 | 0 |
| `StartStream` | звонки и стримы | да | 2 | 0 |
| `StopScreenShare` | звонки и стримы | да | 2 | 0 |
| `SubmitCallAnswer` | звонки и стримы | да | 2 | 0 |
| `SwitchCallRelay` | звонки и стримы | да | 0 | 0 |
| `UnwatchStream` | звонки и стримы | да | 2 | 0 |
| `UpdateStreamQuality` | звонки и стримы | да | 2 | 0 |
| `WatchStream` | звонки и стримы | да | 2 | 0 |
| `ChatAttachments` | чаты | да | 0 | 0 |
| `ChatDelete` | чаты | да | 0 | 0 |
| `ChatEdit` | чаты | да | 0 | 0 |
| `ChatHistory` | чаты | да | 0 | 0 |
| `ChatMarkRead` | чаты | да | 0 | 0 |
| `ChatMessage` | чаты | да | 0 | 0 |
| `ChatPin` | чаты | да | 0 | 0 |
| `ChatPins` | чаты | да | 0 | 0 |
| `ChatReact` | чаты | да | 0 | 0 |
| `ChatRows` | чаты | да | 1 | 0 |
| `ChatSend` | чаты | да | 0 | 0 |
| `ChatThread` | чаты | да | 0 | 0 |
| `Conversations` | чаты | да | 0 | 0 |
| `GetMessages` | чаты | да | 3 | 0 |
| `ListSavedMessages` | чаты | да | 2 | 0 |
| `MessageHandler` | чаты | да | 2 | 0 |
| `OnMessageState` | чаты | да | 2 | 0 |
| `ResetEncryptionSession` | чаты | да | 2 | 0 |
| `SaveMessage` | чаты | да | 2 | 0 |
| `SendMessage` | чаты | да | 2 | 0 |
| `SubscribeConversations` | чаты | да | 0 | 0 |
| `UnsaveMessage` | чаты | да | 2 | 0 |
| `AcceptMessageRequest` | контакты | да | 0 | 0 |
| `BanContact` | контакты | да | 0 | 0 |
| `BlockPeer` | контакты | да | 0 | 0 |
| `CanRing` | контакты | да | 1 | 0 |
| `DeleteContact` | контакты | да | 2 | 0 |
| `DismissMessageRequest` | контакты | да | 0 | 0 |
| `GetBlockedPeers` | контакты | да | 0 | 0 |
| `GetContacts` | контакты | да | 2 | 0 |
| `GetMessageRequests` | контакты | да | 0 | 0 |
| `NewContact` | контакты | да | 2 | 0 |
| `SetContactProfile` | контакты | да | 0 | 0 |
| `UnblockPeer` | контакты | да | 0 | 0 |
| `AcceptFile` | файлы | да | 0 | 0 |
| `CancelFile` | файлы | да | 0 | 0 |
| `DeclineFile` | файлы | да | 0 | 0 |
| `DownloadDir` | файлы | да | 1 | 0 |
| `FileTransferPath` | файлы | да | 0 | 0 |
| `GetFileTransfer` | файлы | да | 0 | 0 |
| `ListFileTransfers` | файлы | да | 0 | 0 |
| `OnFileTransfer` | файлы | да | 2 | 0 |
| `SendFile` | файлы | да | 1 | 0 |
| `SendGroupFile` | файлы | да | 0 | 0 |
| `AddGroupMember` | группы и каналы | да | 0 | 0 |
| `ChatCategoriesSetup` | группы и каналы | да | 0 | 0 |
| `ChatChannelMove` | группы и каналы | да | 0 | 0 |
| `ChatChannelPlace` | группы и каналы | да | 0 | 0 |
| `ChatChannelRemove` | группы и каналы | да | 0 | 0 |
| `ChatChannelSave` | группы и каналы | да | 0 | 0 |
| `ChatChannels` | группы и каналы | да | 0 | 0 |
| `ChatGroupPhoto` | группы и каналы | да | 0 | 0 |
| `ChatMainChannel` | группы и каналы | да | 0 | 0 |
| `ChatMainChannelSave` | группы и каналы | да | 0 | 0 |
| `ChatThreadTitle` | группы и каналы | да | 0 | 0 |
| `ChatThreads` | группы и каналы | да | 0 | 0 |
| `CreateGroup` | группы и каналы | да | 0 | 0 |
| `DeleteGroup` | группы и каналы | да | 0 | 0 |
| `GetGroupInvite` | группы и каналы | да | 0 | 0 |
| `GetGroupMessages` | группы и каналы | да | 0 | 0 |
| `GroupInviteContacts` | группы и каналы | да | 0 | 0 |
| `GroupInviteCreate` | группы и каналы | да | 0 | 0 |
| `GroupInviteRevoke` | группы и каналы | да | 0 | 0 |
| `GroupInvites` | группы и каналы | да | 0 | 0 |
| `GroupJoinDecide` | группы и каналы | да | 0 | 0 |
| `GroupJoinRequests` | группы и каналы | да | 0 | 0 |
| `GroupModerationLog` | группы и каналы | да | 0 | 0 |
| `GroupRoleRemove` | группы и каналы | да | 0 | 0 |
| `GroupRoleSave` | группы и каналы | да | 0 | 0 |
| `GroupRoles` | группы и каналы | да | 0 | 0 |
| `GroupSnapshotExport` | группы и каналы | да | 0 | 0 |
| `GroupSnapshotImport` | группы и каналы | да | 0 | 0 |
| `GroupTimeout` | группы и каналы | да | 0 | 0 |
| `InspectGroupInvite` | группы и каналы | да | 0 | 0 |
| `JoinGroup` | группы и каналы | да | 0 | 0 |
| `LeaveGroup` | группы и каналы | да | 0 | 0 |
| `ListGroupMembers` | группы и каналы | да | 0 | 0 |
| `ListGroups` | группы и каналы | да | 0 | 0 |
| `RemoveGroupMember` | группы и каналы | да | 0 | 0 |
| `SendGroupMessage` | группы и каналы | да | 0 | 0 |
| `SetGroupRole` | группы и каналы | да | 0 | 0 |
| `SubscribeGroupEvents` | группы и каналы | да | 0 | 0 |
| `TransferGroupOwnership` | группы и каналы | да | 0 | 0 |
| `AddBootstrapNode` | сеть и доступ | да | 6 | 2 |
| `AddRelay` | сеть и доступ | да | 1 | 0 |
| `CheckProxy` | сеть и доступ | да | 0 | 0 |
| `ConnectToPeer` | сеть и доступ | да | 8 | 4 |
| `CreatePass` | сеть и доступ | да | 0 | 0 |
| `DeletePass` | сеть и доступ | да | 0 | 0 |
| `ExtendPass` | сеть и доступ | да | 0 | 0 |
| `ForgetInvite` | сеть и доступ | да | 1 | 0 |
| `GetAccessStatus` | сеть и доступ | да | 6 | 1 |
| `GetAnonymity` | сеть и доступ | да | 4 | 0 |
| `GetBootstrapNodes` | сеть и доступ | да | 3 | 2 |
| `GetGuardStatus` | сеть и доступ | да | 8 | 5 |
| `GetNetworkStats` | сеть и доступ | да | 3 | 3 |
| `GetPass` | сеть и доступ | да | 0 | 0 |
| `GetPeers` | сеть и доступ | да | 1 | 1 |
| `GetProxySettings` | сеть и доступ | да | 4 | 0 |
| `GetRelaySettings` | сеть и доступ | да | 2 | 0 |
| `GetRelayStatus` | сеть и доступ | да | 4 | 2 |
| `JoinInvite` | сеть и доступ | да | 3 | 1 |
| `ListPasses` | сеть и доступ | да | 0 | 0 |
| `MeasureUploadCapacity` | сеть и доступ | да | 4 | 2 |
| `OnPeerState` | сеть и доступ | да | 2 | 0 |
| `ParseInvite` | сеть и доступ | да | 0 | 0 |
| `PeerDiscoveryHandler` | сеть и доступ | да | 1 | 0 |
| `PinGuard` | сеть и доступ | да | 7 | 2 |
| `RemoveBootstrapNode` | сеть и доступ | да | 3 | 0 |
| `RemoveJoinedPeer` | сеть и доступ | да | 1 | 0 |
| `RemovePassDevice` | сеть и доступ | да | 0 | 0 |
| `RevokePass` | сеть и доступ | да | 0 | 0 |
| `RotateAccessSecret` | сеть и доступ | да | 1 | 0 |
| `SetAccessMode` | сеть и доступ | да | 4 | 1 |
| `SetAnonymity` | сеть и доступ | да | 7 | 1 |
| `SetGuardMode` | сеть и доступ | да | 5 | 2 |
| `SetProxySettings` | сеть и доступ | да | 5 | 1 |
| `SetRelaySettings` | сеть и доступ | да | 4 | 0 |
| `StartLocalDiscovery` | сеть и доступ | да | 5 | 0 |
| `StartVPNVia` | сеть и доступ | да | 8 | 5 |
| `StopLocalDiscovery` | сеть и доступ | да | 1 | 0 |
| `UnpinGuard` | сеть и доступ | да | 7 | 2 |
| `ChangePassword` | профиль, пароль, настройки | нет | 1 | 0 |
| `GetMyID` | профиль, пароль, настройки | нет | 2 | 0 |
| `GetMyPubKey` | профиль, пароль, настройки | нет | 2 | 0 |
| `GetPrivacy` | профиль, пароль, настройки | нет | 1 | 0 |
| `GetProtocol` | профиль, пароль, настройки | нет | 2 | 0 |
| `GetSettings` | профиль, пароль, настройки | нет | 2 | 0 |
| `IsRegistered` | профиль, пароль, настройки | нет | 1 | 0 |
| `Login` | профиль, пароль, настройки | нет | 1 | 0 |
| `LoginRemembered` | профиль, пароль, настройки | нет | 1 | 0 |
| `Recover` | профиль, пароль, настройки | нет | 1 | 0 |
| `Register` | профиль, пароль, настройки | нет | 1 | 0 |
| `RememberUnlock` | профиль, пароль, настройки | нет | 1 | 0 |
| `RememberedUnlock` | профиль, пароль, настройки | нет | 1 | 0 |
| `SetPrivacy` | профиль, пароль, настройки | нет | 3 | 0 |
| `SetProtocol` | профиль, пароль, настройки | нет | 3 | 0 |
| `SetSettings` | профиль, пароль, настройки | нет | 3 | 0 |
| `StartNode` | профиль, пароль, настройки | да | 4 | 0 |
| `StopNode` | профиль, пароль, настройки | да | 1 | 0 |
| `TriggerPanicButton` | профиль, пароль, настройки | нет | 1 | 0 |
| `VerifyPassword` | профиль, пароль, настройки | нет | 1 | 0 |
| `ConnectionReport` | логи и отчёты | да | 9 | 3 |
| `CheckSites` | VPN, рынок и оплата выхода | да | 2 | 1 |
| `ClearVPNHistory` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `ConnectLastExit` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `ForgetExit` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `GetExitFreePeers` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `GetExitGatewayStatus` | VPN, рынок и оплата выхода | да | 3 | 0 |
| `GetExitPayment` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `GetProbeDomains` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `GetRelayMarket` | VPN, рынок и оплата выхода | да | 2 | 0 |
| `GetSavedExits` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `GetSharingStatus` | VPN, рынок и оплата выхода | да | 5 | 0 |
| `GetSiteLists` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `GetVPNAutoSwitch` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `GetVPNHistory` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `GetVPNStatus` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `KillSwitchOn` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `ListExitNodes` | VPN, рынок и оплата выхода | да | 1 | 1 |
| `PayExit` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `ProbeExitNode` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `RefreshExitNodes` | VPN, рынок и оплата выхода | да | 2 | 0 |
| `SetAutoPay` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `SetCustomSites` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `SetExitFavorite` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `SetExitFreePeers` | VPN, рынок и оплата выхода | да | 2 | 0 |
| `SetExitGatewayEnabled` | VPN, рынок и оплата выхода | да | 2 | 0 |
| `SetExitName` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `SetKillSwitch` | VPN, рынок и оплата выхода | да | 2 | 0 |
| `SetPayAsset` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `SetProbeDomains` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `SetVPNAutoSwitch` | VPN, рынок и оплата выхода | да | 2 | 0 |
| `SetWholeComputer` | VPN, рынок и оплата выхода | да | 5 | 0 |
| `StartSharing` | VPN, рынок и оплата выхода | да | 3 | 0 |
| `StartVPN` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `StartVPNAt` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `StopSharing` | VPN, рынок и оплата выхода | да | 1 | 0 |
| `StopVPN` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `StopVPNForSwitch` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `WholeComputerSupported` | VPN, рынок и оплата выхода | да | 0 | 0 |
| `ExportTonRecoveryPhrase` | кошелёк | нет | 1 | 0 |
| `GetOnRampURL` | кошелёк | нет | 2 | 0 |
| `GetSupport` | кошелёк | нет | 0 | 0 |
| `GetTonBalance` | кошелёк | нет | 0 | 0 |
| `GetTonHistory` | кошелёк | нет | 0 | 0 |
| `GetTonTopUpLink` | кошелёк | нет | 0 | 0 |
| `GetTonWallet` | кошелёк | нет | 0 | 0 |
| `GetVault` | кошелёк | нет | 0 | 0 |
| `GetVoucherEarnings` | кошелёк | нет | 1 | 0 |
| `OwnHTTPClient` | кошелёк | нет | 1 | 0 |
| `PayForTraffic` | кошелёк | нет | 0 | 0 |
| `SendTon` | кошелёк | нет | 0 | 0 |
| `SendUsdt` | кошелёк | нет | 0 | 0 |
| `SetSupportPercent` | кошелёк | нет | 0 | 0 |
| `VaultCancelWithdraw` | кошелёк | нет | 0 | 0 |
| `VaultDeposit` | кошелёк | нет | 0 | 0 |
| `VaultExecuteWithdraw` | кошелёк | нет | 0 | 0 |
| `VaultRequestWithdraw` | кошелёк | нет | 0 | 0 |

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
   обращений стоит в неэкспортированных помощниках (326 методов всего).
