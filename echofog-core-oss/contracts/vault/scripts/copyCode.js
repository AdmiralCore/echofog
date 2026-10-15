// Copies the compiled contract code into the Go wallet package, which embeds it. CI rebuilds the
// contract and fails if the committed copy differs, so the code the app uses is always the code
// in this folder.
const fs = require('fs');
const path = require('path');
fs.copyFileSync(
    path.join(__dirname, '../build/EchoFogVault/tact_EchoFogVault.code.boc'),
    path.join(__dirname, '../../../internal/wallet/tonwallet/vault_code.boc'),
);
