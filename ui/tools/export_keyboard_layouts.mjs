#!/usr/bin/env node
// Exports the keyboard layouts and HID key codes from src/ to a JSON file
// that the device embeds, so typing text over the API uses the same tables
// as the paste dialog. Run with --check to fail when the file is outdated.
import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

import { checkKeyboardLayouts, isTypeable, loadKeyboardLayouts } from "./keyboard_layouts.mjs";

const outFile = fileURLToPath(new URL("../../internal/keyboard/layouts.json", import.meta.url));
const check = process.argv.includes("--check");

function keyInfo(info) {
  const out = { key: String(info.key) };
  if (info.shift) out.shift = true;
  if (info.altRight) out.altRight = true;
  return out;
}

function keyCombo(combo) {
  const out = keyInfo(combo);
  if (combo.deadKey) out.deadKey = true;
  if (combo.accentKey) out.accentKey = keyInfo(combo.accentKey);
  return out;
}

// One entry per line keeps diffs of layout changes readable.
function format({ keys, layouts }) {
  const lines = ["{", '  "keys": {'];
  const keyEntries = Object.entries(keys);
  keyEntries.forEach(([name, code], i) => {
    lines.push(
      `    ${JSON.stringify(name)}: ${String(code)}${i < keyEntries.length - 1 ? "," : ""}`,
    );
  });
  lines.push("  },", '  "layouts": [');
  layouts.forEach((layout, i) => {
    lines.push("    {");
    lines.push(`      "isoCode": ${JSON.stringify(layout.isoCode)},`);
    lines.push(`      "name": ${JSON.stringify(layout.name)},`);
    lines.push('      "chars": {');
    const chars = Object.entries(layout.chars);
    chars.forEach(([char, combo], j) => {
      const sep = j < chars.length - 1 ? "," : "";
      lines.push(`        ${JSON.stringify(char)}: ${JSON.stringify(combo)}${sep}`);
    });
    lines.push("      }");
    lines.push(`    }${i < layouts.length - 1 ? "," : ""}`);
  });
  lines.push("  ]", "}", "");
  return lines.join("\n");
}

const { keyboards, keys } = await loadKeyboardLayouts();
const errors = checkKeyboardLayouts(keyboards, keys);
if (errors.length > 0) {
  console.error(errors.join("\n"));
  process.exit(1);
}

const layouts = keyboards.map(layout => {
  const chars = {};
  for (const [char, combo] of Object.entries(layout.chars)) {
    if (isTypeable(char)) chars[char.normalize("NFC")] = keyCombo(combo);
  }
  return { isoCode: layout.isoCode, name: layout.name, chars };
});

const output = format({ keys, layouts });
if (check) {
  const current = await readFile(outFile, "utf-8").catch(() => "");
  if (current !== output) {
    console.error(`${outFile} is outdated, run: npm run keyboard:export`);
    process.exit(1);
  }
  console.log("the exported keyboard layouts are up to date");
} else {
  await writeFile(outFile, output);
  console.log(`wrote ${layouts.length} layouts to ${outFile}`);
}
