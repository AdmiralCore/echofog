import { CompilerConfig } from '@ton/blueprint';

export const compile: CompilerConfig = {
    lang: 'tact',
    target: 'contracts/echofog_vault.tact',
    options: {
        debug: false,
    },
};
