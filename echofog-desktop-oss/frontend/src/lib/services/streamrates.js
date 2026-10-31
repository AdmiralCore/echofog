// The rates of a stream over the time between two callService.tileStats snapshots: what the stats
// on a tile show and what a stream log records, worked out the same way for both.

/** rates(a, b, own): per-second counts between snapshots a and b of one stream. */
export function rates(a, b, own) {
    const dt = (b.at - a.at) / 1000;
    const per = (x, y) => Math.max(0, Math.round(((y || 0) - (x || 0)) / dt));
    const ab = a.browser || {}, bb = b.browser || {};
    const an = a.node || {}, bn = b.node || {};
    const r = {
        width: bb.width, height: bb.height, impl: bb.impl, hw: bb.hw, limit: bb.limit,
        mbps: ((bb.bytes - ab.bytes) * 8) / dt / 1e6,
        captureFps: Math.round(bb.captureFps || 0),
        encodedFps: per(ab.encoded, bb.encoded),
        sentFps: per(ab.frames, bb.frames),
        receivedFps: per(ab.frames, bb.frames),
        shownFps: per(ab.decoded, bb.decoded),
        droppedFps: per(ab.dropped, bb.dropped),
        packets: per(ab.packets, bb.packets),
        lostPkts: per(ab.lost, bb.lost),
        freezes: bb.freezes || 0, freezeSec: bb.freezeSec || 0, pli: bb.pli || 0, nack: bb.nack || 0,
        codecMs: bb.encoded || bb.decoded ? (((bb.codecTime || 0) - (ab.codecTime || 0)) * 1000) / Math.max(1, (bb.encoded || bb.decoded) - (ab.encoded || ab.decoded)) : 0,
        bufferMs: bb.jbCount > ab.jbCount ? (((bb.jbDelay || 0) - (ab.jbDelay || 0)) * 1000) / (bb.jbCount - ab.jbCount) : 0,
        units: per(own ? an.unitsSent : an.delivered, own ? bn.unitsSent : bn.delivered),
        gaps: per(an.lost, bn.lost), nacked: per(an.nacked, bn.nacked), fec: per(an.recovered, bn.recovered),
        retransmitted: per(an.retransmitted, bn.retransmitted),
        qDropped: per(an.videoDropped, bn.videoDropped), qFailed: per(an.videoFailed, bn.videoFailed),
        netOut: per(an.netDroppedOut, bn.netDroppedOut), netIn: per(an.netDroppedIn, bn.netDroppedIn),
        sendKbps: bn.sendKbps || 0, keyframes: bn.keyframesServed || 0,
    };
    r.fps = own ? r.encodedFps : r.shownFps;
    r.bad = own ? r.qDropped + r.qFailed + r.netOut > 0 : r.lostPkts + r.gaps + r.netIn > 0 || (bb.freezes || 0) > (ab.freezes || 0);
    return r;
}
