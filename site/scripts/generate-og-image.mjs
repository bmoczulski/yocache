// Rasterizes scripts/og-image.svg (the source of truth, hand-authored/exported
// with all text already converted to paths so this needs no fonts installed)
// into public/og-image.png, the file actually served for link previews.
import { readFile, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import sharp from 'sharp';

const scriptsDir = path.dirname(fileURLToPath(import.meta.url));
const svgPath = path.join(scriptsDir, 'og-image.svg');
const pngPath = path.join(scriptsDir, '..', 'public', 'og-image.png');

const svg = await readFile(svgPath);
const png = await sharp(svg).png().toBuffer();
await writeFile(pngPath, png);

console.log(`generated ${path.relative(process.cwd(), pngPath)} from ${path.relative(process.cwd(), svgPath)}`);
