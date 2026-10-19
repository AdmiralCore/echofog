export namespace dto {
	
	export class CallMediaSlotDTO {
	    slot: string;
	    kind: string;
	    peer: string;
	    streamId: string;
	    active: boolean;
	
	    static createFrom(source: any = {}) {
	        return new CallMediaSlotDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.slot = source["slot"];
	        this.kind = source["kind"];
	        this.peer = source["peer"];
	        this.streamId = source["streamId"];
	        this.active = source["active"];
	    }
	}
	export class CallRosterDTO {
	    callId: string;
	    self: string;
	    coordinator: string;
	    members: string[];
	    streams: string[];
	
	    static createFrom(source: any = {}) {
	        return new CallRosterDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.callId = source["callId"];
	        this.self = source["self"];
	        this.coordinator = source["coordinator"];
	        this.members = source["members"];
	        this.streams = source["streams"];
	    }
	}
	export class ContactDTO {
	    peer_id: string;
	    pub_key: string;
	    alias: string;
	    avatar_base64: string;
	    is_online: boolean;
	    last_seen: number;
	
	    static createFrom(source: any = {}) {
	        return new ContactDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peer_id = source["peer_id"];
	        this.pub_key = source["pub_key"];
	        this.alias = source["alias"];
	        this.avatar_base64 = source["avatar_base64"];
	        this.is_online = source["is_online"];
	        this.last_seen = source["last_seen"];
	    }
	}
	export class FileTransferDTO {
	    id: string;
	    peer: string;
	    dir: string;
	    name: string;
	    size: number;
	    mime?: string;
	    state: string;
	    error?: string;
	    path?: string;
	    done: number;
	    rate: number;
	    stalled: boolean;
	    group?: string;
	    members?: number;
	    delivered?: number;
	    fetching?: number;
	    created: number;
	    updated: number;
	
	    static createFrom(source: any = {}) {
	        return new FileTransferDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.peer = source["peer"];
	        this.dir = source["dir"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.mime = source["mime"];
	        this.state = source["state"];
	        this.error = source["error"];
	        this.path = source["path"];
	        this.done = source["done"];
	        this.rate = source["rate"];
	        this.stalled = source["stalled"];
	        this.group = source["group"];
	        this.members = source["members"];
	        this.delivered = source["delivered"];
	        this.fetching = source["fetching"];
	        this.created = source["created"];
	        this.updated = source["updated"];
	    }
	}
	export class MessageDTO {
	    id: string;
	    is_mine: boolean;
	    sender_id: string;
	    target_id: string;
	    target_pub_key?: string;
	    type: number;
	    text?: string;
	    content_hash?: string;
	    metadata?: string;
	    timestamp: number;
	    reply_to_id?: string;
	    app_id?: string;
	    pending?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new MessageDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.is_mine = source["is_mine"];
	        this.sender_id = source["sender_id"];
	        this.target_id = source["target_id"];
	        this.target_pub_key = source["target_pub_key"];
	        this.type = source["type"];
	        this.text = source["text"];
	        this.content_hash = source["content_hash"];
	        this.metadata = source["metadata"];
	        this.timestamp = source["timestamp"];
	        this.reply_to_id = source["reply_to_id"];
	        this.app_id = source["app_id"];
	        this.pending = source["pending"];
	    }
	}
	export class SavedMessageDTO {
	    id: string;
	    chatId: string;
	    chatName: string;
	    msgId: string;
	    sender?: string;
	    mine: boolean;
	    text: string;
	    fileName?: string;
	    fileSize?: number;
	    ts: number;
	    savedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new SavedMessageDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.chatId = source["chatId"];
	        this.chatName = source["chatName"];
	        this.msgId = source["msgId"];
	        this.sender = source["sender"];
	        this.mine = source["mine"];
	        this.text = source["text"];
	        this.fileName = source["fileName"];
	        this.fileSize = source["fileSize"];
	        this.ts = source["ts"];
	        this.savedAt = source["savedAt"];
	    }
	}
	export class StreamQualityDTO {
	    id: string;
	    label: string;
	    width: number;
	    height: number;
	    fps: number;
	    bitrateKbps: number;
	    codec: string;
	    fits: boolean;
	
	    static createFrom(source: any = {}) {
	        return new StreamQualityDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.label = source["label"];
	        this.width = source["width"];
	        this.height = source["height"];
	        this.fps = source["fps"];
	        this.bitrateKbps = source["bitrateKbps"];
	        this.codec = source["codec"];
	        this.fits = source["fits"];
	    }
	}
	export class SupportDTO {
	    available: boolean;
	    percent: number;
	    pendingTon: string;
	    pendingUsdt: string;
	    address: string;
	
	    static createFrom(source: any = {}) {
	        return new SupportDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.available = source["available"];
	        this.percent = source["percent"];
	        this.pendingTon = source["pendingTon"];
	        this.pendingUsdt = source["pendingUsdt"];
	        this.address = source["address"];
	    }
	}
	export class TonBalanceDTO {
	    nano: string;
	    ton: string;
	    deployed: boolean;
	    usdRate: number;
	    usdtAvailable: boolean;
	    usdt: string;
	
	    static createFrom(source: any = {}) {
	        return new TonBalanceDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.nano = source["nano"];
	        this.ton = source["ton"];
	        this.deployed = source["deployed"];
	        this.usdRate = source["usdRate"];
	        this.usdtAvailable = source["usdtAvailable"];
	        this.usdt = source["usdt"];
	    }
	}
	export class TonTxDTO {
	    asset: string;
	    hash: string;
	    time: number;
	    incoming: boolean;
	    ton: string;
	    counterparty: string;
	    comment: string;
	    feeTon: string;
	    failed: boolean;
	
	    static createFrom(source: any = {}) {
	        return new TonTxDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.asset = source["asset"];
	        this.hash = source["hash"];
	        this.time = source["time"];
	        this.incoming = source["incoming"];
	        this.ton = source["ton"];
	        this.counterparty = source["counterparty"];
	        this.comment = source["comment"];
	        this.feeTon = source["feeTon"];
	        this.failed = source["failed"];
	    }
	}
	export class TonWalletDTO {
	    address: string;
	    rawAddress: string;
	    network: string;
	    topUpLink: string;
	    frozen: boolean;
	
	    static createFrom(source: any = {}) {
	        return new TonWalletDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.address = source["address"];
	        this.rawAddress = source["rawAddress"];
	        this.network = source["network"];
	        this.topUpLink = source["topUpLink"];
	        this.frozen = source["frozen"];
	    }
	}
	export class VaultDTO {
	    address: string;
	    deployed: boolean;
	    depositTon: string;
	    closing: boolean;
	    withdrawAt: number;
	    canFinish: boolean;
	    minFirstTon: string;
	
	    static createFrom(source: any = {}) {
	        return new VaultDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.address = source["address"];
	        this.deployed = source["deployed"];
	        this.depositTon = source["depositTon"];
	        this.closing = source["closing"];
	        this.withdrawAt = source["withdrawAt"];
	        this.canFinish = source["canFinish"];
	        this.minFirstTon = source["minFirstTon"];
	    }
	}
	export class VoucherEarningsDTO {
	    owedTon: string;
	    cashedTon: string;
	    quoteNano: number;
	    claimProblem?: string;
	    walletAddr?: string;
	
	    static createFrom(source: any = {}) {
	        return new VoucherEarningsDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.owedTon = source["owedTon"];
	        this.cashedTon = source["cashedTon"];
	        this.quoteNano = source["quoteNano"];
	        this.claimProblem = source["claimProblem"];
	        this.walletAddr = source["walletAddr"];
	    }
	}

}

export namespace logging {
	
	export class Entry {
	    // Go type: time
	    time: any;
	    level: string;
	    message: string;
	    attrs?: Record<string, string>;
	
	    static createFrom(source: any = {}) {
	        return new Entry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.time = this.convertValues(source["time"], null);
	        this.level = source["level"];
	        this.message = source["message"];
	        this.attrs = source["attrs"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace node {
	
	export class BootstrapNode {
	    addr: string;
	    peerId: string;
	    link: string;
	    connected: boolean;
	
	    static createFrom(source: any = {}) {
	        return new BootstrapNode(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.addr = source["addr"];
	        this.peerId = source["peerId"];
	        this.link = source["link"];
	        this.connected = source["connected"];
	    }
	}
	export class BootstrapStatus {
	    builtIn: BootstrapNode[];
	    custom: BootstrapNode[];
	    peers: number;
	
	    static createFrom(source: any = {}) {
	        return new BootstrapStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.builtIn = this.convertValues(source["builtIn"], BootstrapNode);
	        this.custom = this.convertValues(source["custom"], BootstrapNode);
	        this.peers = source["peers"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ExitNode {
	    peerId: string;
	    pricePerMb: number;
	    freeTestKb: number;
	    claimedKbps: number;
	    measuredKbps: number;
	    latencyMs: number;
	    score: number;
	    probes: number;
	    successes: number;
	    domainsTried: number;
	    domainsOk: number;
	    lastProbe: number;
	    paymentRequired: boolean;
	    connected: boolean;
	    expiresAt: number;
	
	    static createFrom(source: any = {}) {
	        return new ExitNode(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peerId = source["peerId"];
	        this.pricePerMb = source["pricePerMb"];
	        this.freeTestKb = source["freeTestKb"];
	        this.claimedKbps = source["claimedKbps"];
	        this.measuredKbps = source["measuredKbps"];
	        this.latencyMs = source["latencyMs"];
	        this.score = source["score"];
	        this.probes = source["probes"];
	        this.successes = source["successes"];
	        this.domainsTried = source["domainsTried"];
	        this.domainsOk = source["domainsOk"];
	        this.lastProbe = source["lastProbe"];
	        this.paymentRequired = source["paymentRequired"];
	        this.connected = source["connected"];
	        this.expiresAt = source["expiresAt"];
	    }
	}
	export class ExitPaymentStatus {
	    active: boolean;
	    canPay: boolean;
	    pricePerGb: string;
	    remainingGb: number;
	    needsPayment: boolean;
	    autoPay: boolean;
	    chunkUsdt: string;
	    chunkGb: number;
	    dailyCapUsdt: string;
	    payAsset: string;
	    takesTon: boolean;
	    chunkTon: string;
	    spentTodayUsdt: string;
	    pending: boolean;
	    note: string;
	    lastError: string;
	    paysByVault: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ExitPaymentStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.active = source["active"];
	        this.canPay = source["canPay"];
	        this.pricePerGb = source["pricePerGb"];
	        this.remainingGb = source["remainingGb"];
	        this.needsPayment = source["needsPayment"];
	        this.autoPay = source["autoPay"];
	        this.chunkUsdt = source["chunkUsdt"];
	        this.chunkGb = source["chunkGb"];
	        this.dailyCapUsdt = source["dailyCapUsdt"];
	        this.payAsset = source["payAsset"];
	        this.takesTon = source["takesTon"];
	        this.chunkTon = source["chunkTon"];
	        this.spentTodayUsdt = source["spentTodayUsdt"];
	        this.pending = source["pending"];
	        this.note = source["note"];
	        this.lastError = source["lastError"];
	        this.paysByVault = source["paysByVault"];
	    }
	}
	export class GroupCall {
	    callId: string;
	    coordinator: string;
	    from: string;
	    startedAt: number;
	    lastSeen: number;
	
	    static createFrom(source: any = {}) {
	        return new GroupCall(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.callId = source["callId"];
	        this.coordinator = source["coordinator"];
	        this.from = source["from"];
	        this.startedAt = source["startedAt"];
	        this.lastSeen = source["lastSeen"];
	    }
	}
	export class GroupDTO {
	    id: string;
	    title: string;
	    description: string;
	    kind: string;
	    readOnly: boolean;
	    creator: string;
	    createdAt: number;
	    members: number;
	    myRole: string;
	
	    static createFrom(source: any = {}) {
	        return new GroupDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.description = source["description"];
	        this.kind = source["kind"];
	        this.readOnly = source["readOnly"];
	        this.creator = source["creator"];
	        this.createdAt = source["createdAt"];
	        this.members = source["members"];
	        this.myRole = source["myRole"];
	    }
	}
	export class GroupFileDTO {
	    id: string;
	    name: string;
	    size: number;
	    mime?: string;
	
	    static createFrom(source: any = {}) {
	        return new GroupFileDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.mime = source["mime"];
	    }
	}
	export class GroupMemberDTO {
	    peerId: string;
	    role: string;
	    joinedAt: number;
	    isMe: boolean;
	
	    static createFrom(source: any = {}) {
	        return new GroupMemberDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peerId = source["peerId"];
	        this.role = source["role"];
	        this.joinedAt = source["joinedAt"];
	        this.isMe = source["isMe"];
	    }
	}
	export class GroupMessageDTO {
	    id: string;
	    groupId: string;
	    sender: string;
	    text: string;
	    timestamp: number;
	    isMine: boolean;
	    delivered: number;
	    queued: number;
	    failed: number;
	    file?: GroupFileDTO;
	
	    static createFrom(source: any = {}) {
	        return new GroupMessageDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.groupId = source["groupId"];
	        this.sender = source["sender"];
	        this.text = source["text"];
	        this.timestamp = source["timestamp"];
	        this.isMine = source["isMine"];
	        this.delivered = source["delivered"];
	        this.queued = source["queued"];
	        this.failed = source["failed"];
	        this.file = this.convertValues(source["file"], GroupFileDTO);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ProbeReport {
	    ok: boolean;
	    latencyMs: number;
	    measuredKbps: number;
	    domainsTried: number;
	    domainsOk: number;
	    paymentRequired: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ProbeReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.latencyMs = source["latencyMs"];
	        this.measuredKbps = source["measuredKbps"];
	        this.domainsTried = source["domainsTried"];
	        this.domainsOk = source["domainsOk"];
	        this.paymentRequired = source["paymentRequired"];
	    }
	}
	export class Reach {
	    ok: boolean;
	    ms: number;
	    reason?: string;
	
	    static createFrom(source: any = {}) {
	        return new Reach(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.ms = source["ms"];
	        this.reason = source["reason"];
	    }
	}
	export class RelaySettingsDTO {
	    server: boolean;
	    private: boolean;
	    signalingOnly: boolean;
	    allowedPeers: string[];
	    relays: string[];
	    listenPort: number;
	
	    static createFrom(source: any = {}) {
	        return new RelaySettingsDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.server = source["server"];
	        this.private = source["private"];
	        this.signalingOnly = source["signalingOnly"];
	        this.allowedPeers = source["allowedPeers"];
	        this.relays = source["relays"];
	        this.listenPort = source["listenPort"];
	    }
	}
	export class RelayUse {
	    addr: string;
	    peerId: string;
	    connected: boolean;
	
	    static createFrom(source: any = {}) {
	        return new RelayUse(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.addr = source["addr"];
	        this.peerId = source["peerId"];
	        this.connected = source["connected"];
	    }
	}
	export class RelayStatus {
	    settings: RelaySettingsDTO;
	    chosen: boolean;
	    link: string;
	    addr: string;
	    nat: string;
	    mapped: boolean;
	    reason: string;
	    code: string;
	    restartNeeded: boolean;
	    activeCircuits: number;
	    bytesRouted: number;
	    forwarded: number;
	    accessRefused: number;
	    using: RelayUse[];
	
	    static createFrom(source: any = {}) {
	        return new RelayStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.settings = this.convertValues(source["settings"], RelaySettingsDTO);
	        this.chosen = source["chosen"];
	        this.link = source["link"];
	        this.addr = source["addr"];
	        this.nat = source["nat"];
	        this.mapped = source["mapped"];
	        this.reason = source["reason"];
	        this.code = source["code"];
	        this.restartNeeded = source["restartNeeded"];
	        this.activeCircuits = source["activeCircuits"];
	        this.bytesRouted = source["bytesRouted"];
	        this.forwarded = source["forwarded"];
	        this.accessRefused = source["accessRefused"];
	        this.using = this.convertValues(source["using"], RelayUse);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class SettingsDTO {
	    directSocket: boolean;
	    socketBufferMb: number;
	
	    static createFrom(source: any = {}) {
	        return new SettingsDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.directSocket = source["directSocket"];
	        this.socketBufferMb = source["socketBufferMb"];
	    }
	}
	export class SharingConfig {
	    pricePerMb: number;
	    freeTestKb: number;
	    uploadKbps: number;
	    freePeers: string[];
	
	    static createFrom(source: any = {}) {
	        return new SharingConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.pricePerMb = source["pricePerMb"];
	        this.freeTestKb = source["freeTestKb"];
	        this.uploadKbps = source["uploadKbps"];
	        this.freePeers = source["freePeers"];
	    }
	}
	export class SharingStatus {
	    enabled: boolean;
	    config: SharingConfig;
	    lastPublish: number;
	    expiresAt: number;
	    lastError: string;
	    accepted: number;
	    refused: number;
	    refusedBy: Record<string, number>;
	    bytes: number;
	    paymentsReady: boolean;
	    payAddress: string;
	    earnedUsdt: string;
	
	    static createFrom(source: any = {}) {
	        return new SharingStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.config = this.convertValues(source["config"], SharingConfig);
	        this.lastPublish = source["lastPublish"];
	        this.expiresAt = source["expiresAt"];
	        this.lastError = source["lastError"];
	        this.accepted = source["accepted"];
	        this.refused = source["refused"];
	        this.refusedBy = source["refusedBy"];
	        this.bytes = source["bytes"];
	        this.paymentsReady = source["paymentsReady"];
	        this.payAddress = source["payAddress"];
	        this.earnedUsdt = source["earnedUsdt"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SiteCheck {
	    host: string;
	    direct: Reach;
	    exit?: Reach;
	
	    static createFrom(source: any = {}) {
	        return new SiteCheck(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.host = source["host"];
	        this.direct = this.convertValues(source["direct"], Reach);
	        this.exit = this.convertValues(source["exit"], Reach);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class VPNSession {
	    peerId: string;
	    addr?: string;
	    startedAt: number;
	    endedAt?: number;
	    updatedAt: number;
	    readyMs?: number;
	    bytesRx: number;
	    bytesTx: number;
	    refused: number;
	    lastState?: string;
	    pricePerMb: number;
	    endReason?: string;
	    active: boolean;
	
	    static createFrom(source: any = {}) {
	        return new VPNSession(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peerId = source["peerId"];
	        this.addr = source["addr"];
	        this.startedAt = source["startedAt"];
	        this.endedAt = source["endedAt"];
	        this.updatedAt = source["updatedAt"];
	        this.readyMs = source["readyMs"];
	        this.bytesRx = source["bytesRx"];
	        this.bytesTx = source["bytesTx"];
	        this.refused = source["refused"];
	        this.lastState = source["lastState"];
	        this.pricePerMb = source["pricePerMb"];
	        this.endReason = source["endReason"];
	        this.active = source["active"];
	    }
	}
	export class VPNSwitch {
	    from: string;
	    to: string;
	    reason: string;
	    at: number;
	
	    static createFrom(source: any = {}) {
	        return new VPNSwitch(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.from = source["from"];
	        this.to = source["to"];
	        this.reason = source["reason"];
	        this.at = source["at"];
	    }
	}
	export class VPNStatus {
	    running: boolean;
	    listenAddr: string;
	    relayPeerId: string;
	    activeConns: number;
	    bytesRx: number;
	    bytesTx: number;
	    tunnelReady: boolean;
	    tunnelNotice: string;
	    exitState: string;
	    autoSwitch: boolean;
	    searching: boolean;
	    switches: number;
	    lastSwitch: VPNSwitch;
	    refused: number;
	    lastRefusalReason: string;
	    lastRefusalHost: string;
	    lastRefusalAt: number;
	    wholeComputer: boolean;
	    wholeComputerSupported: boolean;
	    wholeComputerError: string;
	
	    static createFrom(source: any = {}) {
	        return new VPNStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.listenAddr = source["listenAddr"];
	        this.relayPeerId = source["relayPeerId"];
	        this.activeConns = source["activeConns"];
	        this.bytesRx = source["bytesRx"];
	        this.bytesTx = source["bytesTx"];
	        this.tunnelReady = source["tunnelReady"];
	        this.tunnelNotice = source["tunnelNotice"];
	        this.exitState = source["exitState"];
	        this.autoSwitch = source["autoSwitch"];
	        this.searching = source["searching"];
	        this.switches = source["switches"];
	        this.lastSwitch = this.convertValues(source["lastSwitch"], VPNSwitch);
	        this.refused = source["refused"];
	        this.lastRefusalReason = source["lastRefusalReason"];
	        this.lastRefusalHost = source["lastRefusalHost"];
	        this.lastRefusalAt = source["lastRefusalAt"];
	        this.wholeComputer = source["wholeComputer"];
	        this.wholeComputerSupported = source["wholeComputerSupported"];
	        this.wholeComputerError = source["wholeComputerError"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

