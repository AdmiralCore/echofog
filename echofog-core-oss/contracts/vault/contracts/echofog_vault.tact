// EchoFogVault: one user's TON deposit for paid EchoFog proxy nodes.
//
// Every user who pays for traffic has their own copy of this contract. Its address is fixed by the
// code and by (owner, pubKey), so anyone can compute it offline, and a node that derives it itself
// knows it talks to the real code and not to a look-alike with lying getters.
//
// How money moves:
//   1. The owner deposits TON (Deposit, an empty transfer or any text comment).
//   2. While using a node, the owner's app signs vouchers off-chain: "node N may take, in total,
//      `cumulative` nanoton from this vault". Each new voucher replaces the previous one; the node
//      only keeps the latest.
//   3. When it is worth the gas, the node sends Claim with the latest voucher. The contract pays the
//      difference between `cumulative` and what that node already took, as far as the deposit
//      covers it, and remembers the new total. An old or repeated voucher pays nothing.
//   4. To get the rest back, the owner sends RequestWithdraw, waits 24 hours (nodes can still claim
//      what the owner already signed for) and sends ExecuteWithdraw.
//
// There is no fee, no admin, no upgrade and no self-destruct. The contract never deletes itself:
// if it did, a redeploy at the same address would start with an empty `claimed` map and every old
// voucher could be cashed a second time.
//
// Voucher cell (signed with ed25519 over its hash, the way TVM CHKSIGNU expects):
//   magic:uint32 = 0x45465631 ("EFV1")   keeps voucher signatures apart from wallet signatures
//   globalId:int32                       -239 mainnet, -3 testnet: a testnet voucher is useless on mainnet
//   vault:MsgAddress                     this contract: a voucher for another user's vault is useless here
//   node:MsgAddress                      who is paid
//   cumulative:Coins                     total nanoton this node may take, ever

const VOUCHER_MAGIC: Int = 0x45465631;

// How long the owner waits between RequestWithdraw and ExecuteWithdraw. Nodes claim during it.
const WITHDRAW_COOLDOWN: Int = 86400;

// Kept on the contract for storage fees; never part of the deposit and never paid out.
const STORAGE_RESERVE: Int = ton("0.005");

// Gas a Deposit may use from the contract balance before it is credited.
const DEPOSIT_GAS: Int = ton("0.003");

// A claim must bring at least this much for gas; what it does not use goes back with the payout.
const MIN_CLAIM_VALUE: Int = ton("0.01");

// Exit codes. They show up in the failed transaction, and the Go node reads them.
const ERR_BAD_VOUCHER: Int = 1000;     // not a voucher cell
const ERR_WRONG_NETWORK: Int = 1001;   // signed for the other TON network
const ERR_WRONG_VAULT: Int = 1002;     // signed for another vault
const ERR_BAD_SIGNATURE: Int = 1003;   // not signed by this vault's key
const ERR_NOTHING_TO_CLAIM: Int = 1004; // this voucher (or a newer one) was already paid
const ERR_EMPTY_DEPOSIT: Int = 1005;   // nothing left to pay from
const ERR_NOT_OWNER: Int = 1006;
const ERR_NO_WITHDRAW: Int = 1007;     // ExecuteWithdraw without RequestWithdraw
const ERR_COOLDOWN: Int = 1008;        // the 24 hours are not over yet
const ERR_LOW_GAS: Int = 1009;         // claim brought less than MIN_CLAIM_VALUE
const ERR_TOO_SMALL: Int = 1010;       // deposit does not even cover the storage reserve

message(0x6566d001) Deposit {
    queryId: Int as uint64;
}

message(0x6566d002) Claim {
    queryId: Int as uint64;
    voucher: Cell;
    signature: Slice as bytes64;
}

message(0x6566d003) RequestWithdraw {
    queryId: Int as uint64;
}

message(0x6566d004) CancelWithdraw {
    queryId: Int as uint64;
}

message(0x6566d005) ExecuteWithdraw {
    queryId: Int as uint64;
}

struct VaultState {
    owner: Address;
    pubKey: Int as uint256;
    deposit: Int as coins;    // what nodes can still be paid from
    withdrawAt: Int as uint32; // 0: no withdrawal pending; else the time ExecuteWithdraw is allowed
    now: Int as uint32;       // chain time of the read, so a client can tell how long is left
}

// All state lives in the contract parameters, so the initial data cell is just
// owner | pubKey | deposit=0 | withdrawAt=0 | claimed=empty and is easy to rebuild outside Tact.
contract EchoFogVault(
    owner: Address,               // the only address that can withdraw
    pubKey: Int as uint256,       // ed25519 key that signs vouchers
    deposit: Int as coins,        // nanoton available to nodes
    withdrawAt: Int as uint32,    // see VaultState
    claimed: map<Address, Int as coins>, // node -> total already paid to it
) {
    // ---- deposits ----

    receive(msg: Deposit) {
        self.credit();
    }

    // A plain transfer, for example from an exchange or another wallet app.
    receive() {
        self.credit();
    }

    // A transfer with a text comment is a deposit as well.
    receive(comment: String) {
        self.credit();
    }

    // Credits the incoming value, minus whatever keeps STORAGE_RESERVE + DEPOSIT_GAS on the contract
    // beside the deposit. The first deposit (the deploy) gives up about 0.008 TON for that; a later
    // one only the gas the previous transaction used, a fraction of a cent. So each deposit pays
    // its own network fee and the deposit itself is never eaten by fees.
    fun credit() {
        let value = context().value;
        let free = myBalance() - self.deposit - STORAGE_RESERVE - DEPOSIT_GAS;
        let credited = min(value, free);
        throwUnless(ERR_TOO_SMALL, credited > 0);
        self.deposit += credited;
    }

    // ---- node payouts ----

    receive(msg: Claim) {
        throwUnless(ERR_LOW_GAS, context().value >= MIN_CLAIM_VALUE);

        let s = msg.voucher.beginParse();
        throwUnless(ERR_BAD_VOUCHER, s.loadUint(32) == VOUCHER_MAGIC);
        throwUnless(ERR_WRONG_NETWORK, s.loadInt(32) == globalId());
        throwUnless(ERR_WRONG_VAULT, s.loadAddress() == myAddress());
        let node = s.loadAddress();
        let cumulative = s.loadCoins();
        s.endParse();
        throwUnless(ERR_BAD_SIGNATURE, checkSignature(msg.voucher.hash(), msg.signature, self.pubKey));

        let before = self.claimed.get(node);
        let already = 0;
        if (before != null) {
            already = before!!;
        }
        let delta = cumulative - already;
        throwUnless(ERR_NOTHING_TO_CLAIM, delta > 0);

        // Partial payout: if the deposit is short, pay what is there and count only that. The
        // same voucher then pays the rest after the owner tops up.
        let paid = min(delta, self.deposit);
        throwUnless(ERR_EMPTY_DEPOSIT, paid > 0);
        self.deposit -= paid;
        self.claimed.set(node, already + paid);

        // The node gets the payout plus whatever gas the claim did not use. Anyone may submit a
        // claim, but the money always goes to the node named in the voucher.
        send(SendParameters {
            to: node,
            value: paid,
            mode: SendRemainingValue | SendBounceIfActionFail,
            bounce: false,
            body: "EchoFog payout".asComment(),
        });
    }

    // ---- withdrawal ----

    receive(msg: RequestWithdraw) {
        self.requireOwner();
        self.withdrawAt = now() + WITHDRAW_COOLDOWN;
        cashback(sender());
    }

    receive(msg: CancelWithdraw) {
        self.requireOwner();
        self.withdrawAt = 0;
        cashback(sender());
    }

    receive(msg: ExecuteWithdraw) {
        self.requireOwner();
        throwUnless(ERR_NO_WITHDRAW, self.withdrawAt != 0);
        throwUnless(ERR_COOLDOWN, now() >= self.withdrawAt);
        let amount = self.deposit;
        self.deposit = 0;
        self.withdrawAt = 0;
        // The state (and so `claimed`) stays: the contract is never destroyed.
        send(SendParameters {
            to: self.owner,
            value: amount,
            mode: SendRemainingValue | SendBounceIfActionFail,
            bounce: false,
            body: "EchoFog vault withdrawal".asComment(),
        });
    }

    fun requireOwner() {
        throwUnless(ERR_NOT_OWNER, sender() == self.owner);
    }

    // ---- getters ----

    get fun state(): VaultState {
        return VaultState {
            owner: self.owner,
            pubKey: self.pubKey,
            deposit: self.deposit,
            withdrawAt: self.withdrawAt,
            now: now(),
        };
    }

    // Total already paid to node (0 if never).
    get fun claimedBy(node: Address): Int {
        let v = self.claimed.get(node);
        if (v == null) {
            return 0;
        }
        return v!!;
    }

    get fun version(): Int {
        return 1;
    }
}

// The TON network this contract runs on (config param 19).
fun globalId(): Int {
    return getConfigParam(19)!!.beginParse().loadInt(32);
}
