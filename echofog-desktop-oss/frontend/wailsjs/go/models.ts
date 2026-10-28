export namespace contacts {
	
	export class Blocked {
	    peer_id: string;
	    alias?: string;
	    at: number;
	
	    static createFrom(source: any = {}) {
	        return new Blocked(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peer_id = source["peer_id"];
	        this.alias = source["alias"];
	        this.at = source["at"];
	    }
	}
	export class Request {
	    peer_id: string;
	    pub_key?: string;
	    first: number;
	    last: number;
	    count: number;
	
	    static createFrom(source: any = {}) {
	        return new Request(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peer_id = source["peer_id"];
	        this.pub_key = source["pub_key"];
	        this.first = source["first"];
	        this.last = source["last"];
	        this.count = source["count"];
	    }
	}

}

export namespace convlog {
	
	export class FileRef {
	    id: string;
	    name: string;
	    size: number;
	    mime?: string;
	
	    static createFrom(source: any = {}) {
	        return new FileRef(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.mime = source["mime"];
	    }
	}
	export class MsgCan {
	    edit: boolean;
	    delete: boolean;
	    pin: boolean;
	    react: boolean;
	    reply: boolean;
	
	    static createFrom(source: any = {}) {
	        return new MsgCan(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.edit = source["edit"];
	        this.delete = source["delete"];
	        this.pin = source["pin"];
	        this.react = source["react"];
	        this.reply = source["reply"];
	    }
	}
	export class Reaction {
	    emoji: string;
	    count: number;
	    by: string[];
	    mine: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Reaction(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.emoji = source["emoji"];
	        this.count = source["count"];
	        this.by = source["by"];
	        this.mine = source["mine"];
	    }
	}
	export class Message {
	    id: string;
	    conv: string;
	    author: string;
	    mine: boolean;
	    ts: number;
	    lamport: number;
	    type: number;
	    text: string;
	    metadata?: string;
	    replyTo?: string;
	    thread?: string;
	    channel?: string;
	    edited: boolean;
	    editedAt?: number;
	    deleted: boolean;
	    deletedBy?: string;
	    withheld?: boolean;
	    pinned: boolean;
	    pinnedBy?: string;
	    pinnedAt?: number;
	    reactions: Reaction[];
	    replies: number;
	    lastReplyAt?: number;
	    legacy?: boolean;
	    state?: string;
	    kind: string;
	    file?: FileRef;
	    can: MsgCan;
	
	    static createFrom(source: any = {}) {
	        return new Message(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.conv = source["conv"];
	        this.author = source["author"];
	        this.mine = source["mine"];
	        this.ts = source["ts"];
	        this.lamport = source["lamport"];
	        this.type = source["type"];
	        this.text = source["text"];
	        this.metadata = source["metadata"];
	        this.replyTo = source["replyTo"];
	        this.thread = source["thread"];
	        this.channel = source["channel"];
	        this.edited = source["edited"];
	        this.editedAt = source["editedAt"];
	        this.deleted = source["deleted"];
	        this.deletedBy = source["deletedBy"];
	        this.withheld = source["withheld"];
	        this.pinned = source["pinned"];
	        this.pinnedBy = source["pinnedBy"];
	        this.pinnedAt = source["pinnedAt"];
	        this.reactions = this.convertValues(source["reactions"], Reaction);
	        this.replies = source["replies"];
	        this.lastReplyAt = source["lastReplyAt"];
	        this.legacy = source["legacy"];
	        this.state = source["state"];
	        this.kind = source["kind"];
	        this.file = this.convertValues(source["file"], FileRef);
	        this.can = this.convertValues(source["can"], MsgCan);
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
	    state?: string;
	    request?: boolean;
	
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
	        this.state = source["state"];
	        this.request = source["request"];
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

export namespace main {
	
	export class UpdateInfo {
	    current: string;
	    latest?: string;
	    url?: string;
	    newer: boolean;
	
	    static createFrom(source: any = {}) {
	        return new UpdateInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.current = source["current"];
	        this.latest = source["latest"];
	        this.url = source["url"];
	        this.newer = source["newer"];
	    }
	}

}

export namespace node {
	
	export class JoinedPeer {
	    peerId: string;
	    joinedAt: number;
	    connected: boolean;
	
	    static createFrom(source: any = {}) {
	        return new JoinedPeer(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peerId = source["peerId"];
	        this.joinedAt = source["joinedAt"];
	        this.connected = source["connected"];
	    }
	}
	export class AccessStatus {
	    mode: string;
	    nextMode?: string;
	    link: string;
	    linkError?: string;
	    secretFromConfig: boolean;
	    joined: JoinedPeer[];
	    network: string;
	
	    static createFrom(source: any = {}) {
	        return new AccessStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.mode = source["mode"];
	        this.nextMode = source["nextMode"];
	        this.link = source["link"];
	        this.linkError = source["linkError"];
	        this.secretFromConfig = source["secretFromConfig"];
	        this.joined = this.convertValues(source["joined"], JoinedPeer);
	        this.network = source["network"];
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
	export class GroupCall {
	    room?: string;
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
	        this.room = source["room"];
	        this.callId = source["callId"];
	        this.coordinator = source["coordinator"];
	        this.from = source["from"];
	        this.startedAt = source["startedAt"];
	        this.lastSeen = source["lastSeen"];
	    }
	}
	export class ChannelDTO {
	    id: string;
	    name: string;
	    voice: boolean;
	    unread: number;
	    lastTs: number;
	    call?: GroupCall;
	
	    static createFrom(source: any = {}) {
	        return new ChannelDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.voice = source["voice"];
	        this.unread = source["unread"];
	        this.lastTs = source["lastTs"];
	        this.call = this.convertValues(source["call"], GroupCall);
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
	export class ChatPage {
	    messages: convlog.Message[];
	    next: string;
	
	    static createFrom(source: any = {}) {
	        return new ChatPage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.messages = this.convertValues(source["messages"], convlog.Message);
	        this.next = source["next"];
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
	export class ChatRef {
	    kind: string;
	    id: string;
	    channel?: string;
	
	    static createFrom(source: any = {}) {
	        return new ChatRef(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.id = source["id"];
	        this.channel = source["channel"];
	    }
	}
	export class ConvCan {
	    send: boolean;
	    files: boolean;
	    react: boolean;
	    pin: boolean;
	    manage: boolean;
	    invite: boolean;
	    call: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ConvCan(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.send = source["send"];
	        this.files = source["files"];
	        this.react = source["react"];
	        this.pin = source["pin"];
	        this.manage = source["manage"];
	        this.invite = source["invite"];
	        this.call = source["call"];
	    }
	}
	export class ConversationDTO {
	    ref: ChatRef;
	    conv: string;
	    title?: string;
	    last?: convlog.Message;
	    lastTs: number;
	    unread: number;
	    request: boolean;
	    upgraded: boolean;
	    kind: string;
	    name: string;
	    peerId?: string;
	    pubKey?: string;
	    online: boolean;
	    blocked: boolean;
	    role?: string;
	    members?: number;
	    can: ConvCan;
	
	    static createFrom(source: any = {}) {
	        return new ConversationDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ref = this.convertValues(source["ref"], ChatRef);
	        this.conv = source["conv"];
	        this.title = source["title"];
	        this.last = this.convertValues(source["last"], convlog.Message);
	        this.lastTs = source["lastTs"];
	        this.unread = source["unread"];
	        this.request = source["request"];
	        this.upgraded = source["upgraded"];
	        this.kind = source["kind"];
	        this.name = source["name"];
	        this.peerId = source["peerId"];
	        this.pubKey = source["pubKey"];
	        this.online = source["online"];
	        this.blocked = source["blocked"];
	        this.role = source["role"];
	        this.members = source["members"];
	        this.can = this.convertValues(source["can"], ConvCan);
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
	    canKick: boolean;
	    canBan: boolean;
	    roles?: string[];
	    canTransfer: boolean;
	
	    static createFrom(source: any = {}) {
	        return new GroupMemberDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peerId = source["peerId"];
	        this.role = source["role"];
	        this.joinedAt = source["joinedAt"];
	        this.isMe = source["isMe"];
	        this.canKick = source["canKick"];
	        this.canBan = source["canBan"];
	        this.roles = source["roles"];
	        this.canTransfer = source["canTransfer"];
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
	    verified: boolean;
	
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
	        this.verified = source["verified"];
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
	export class InviteInfo {
	    peerId: string;
	    private: boolean;
	    addrs: number;
	    relays: number;
	    protocol: number;
	
	    static createFrom(source: any = {}) {
	        return new InviteInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peerId = source["peerId"];
	        this.private = source["private"];
	        this.addrs = source["addrs"];
	        this.relays = source["relays"];
	        this.protocol = source["protocol"];
	    }
	}
	
	export class Privacy {
	    messages: string;
	    calls: string;
	
	    static createFrom(source: any = {}) {
	        return new Privacy(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.messages = source["messages"];
	        this.calls = source["calls"];
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
	    code?: string;
	    reason?: string;
	
	    static createFrom(source: any = {}) {
	        return new Reach(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.ms = source["ms"];
	        this.code = source["code"];
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
	
	export class SavedExit {
	    peerId: string;
	    name?: string;
	    favorite: boolean;
	    addr?: string;
	    sessions: number;
	    bytesRx: number;
	    bytesTx: number;
	    refused: number;
	    refusedBy?: Record<string, number>;
	    lastUsed: number;
	    lastReadyMs?: number;
	    lastState?: string;
	    pricePerMb: number;
	    priceKnown: boolean;
	    remaining?: number;
	    remainingAt?: number;
	    active: boolean;
	    countryCode?: string;
	    city?: string;
	
	    static createFrom(source: any = {}) {
	        return new SavedExit(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peerId = source["peerId"];
	        this.name = source["name"];
	        this.favorite = source["favorite"];
	        this.addr = source["addr"];
	        this.sessions = source["sessions"];
	        this.bytesRx = source["bytesRx"];
	        this.bytesTx = source["bytesTx"];
	        this.refused = source["refused"];
	        this.refusedBy = source["refusedBy"];
	        this.lastUsed = source["lastUsed"];
	        this.lastReadyMs = source["lastReadyMs"];
	        this.lastState = source["lastState"];
	        this.pricePerMb = source["pricePerMb"];
	        this.priceKnown = source["priceKnown"];
	        this.remaining = source["remaining"];
	        this.remainingAt = source["remainingAt"];
	        this.active = source["active"];
	        this.countryCode = source["countryCode"];
	        this.city = source["city"];
	    }
	}
	export class SendRequest {
	    text: string;
	    replyTo?: string;
	    thread?: string;
	
	    static createFrom(source: any = {}) {
	        return new SendRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.text = source["text"];
	        this.replyTo = source["replyTo"];
	        this.thread = source["thread"];
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
	export class SiteLists {
	    selected: string[];
	    custom: string[];
	    chosen: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SiteLists(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.selected = source["selected"];
	        this.custom = source["custom"];
	        this.chosen = source["chosen"];
	    }
	}
	export class VPNSession {
	    id: string;
	    peerId: string;
	    addr?: string;
	    startedAt: number;
	    endedAt?: number;
	    updatedAt: number;
	    readyMs?: number;
	    bytesRx: number;
	    bytesTx: number;
	    refused: number;
	    refusedBy?: Record<string, number>;
	    lastState?: string;
	    pricePerMb: number;
	    priceKnown: boolean;
	    remaining?: number;
	    remainingAt?: number;
	    endReason?: string;
	    active: boolean;
	
	    static createFrom(source: any = {}) {
	        return new VPNSession(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.peerId = source["peerId"];
	        this.addr = source["addr"];
	        this.startedAt = source["startedAt"];
	        this.endedAt = source["endedAt"];
	        this.updatedAt = source["updatedAt"];
	        this.readyMs = source["readyMs"];
	        this.bytesRx = source["bytesRx"];
	        this.bytesTx = source["bytesTx"];
	        this.refused = source["refused"];
	        this.refusedBy = source["refusedBy"];
	        this.lastState = source["lastState"];
	        this.pricePerMb = source["pricePerMb"];
	        this.priceKnown = source["priceKnown"];
	        this.remaining = source["remaining"];
	        this.remainingAt = source["remainingAt"];
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

