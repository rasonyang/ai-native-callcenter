// Writes a flow spec whose text is as wide as a spec can get, for checking that
// a table never lets row content size it (issue #377). Run it with:
//
//     node scripts/make-wide-flow.mjs [out.json]      (stdout without a path)
//
// Nothing here is imported by the app or by a test; the output is loaded by
// hand (`aicc flowadd`, or a publish through the API) into a dev stack. The
// output is deterministic: no randomness, same bytes on every run. It holds
//
//   - a persona line of 6,000 characters with no space, and a Chinese one of
//     3,000 characters with no punctuation, so nothing can wrap;
//   - an HTTP tool whose path is a 2,000-character URL;
//   - 400 phases, so the pretty-printed spec is several thousand lines long.
//
// The spec passes flow.Load, so it publishes like any other.

import { writeFileSync } from 'node:fs'

const PHASES = 400
const EN_LEN = 6000
const ZH_LEN = 3000
const PATH_LEN = 2000

// Repeat `unit` until the string is exactly `length` characters.
const fill = (unit, length) => unit.repeat(Math.ceil(length / [...unit].length)).slice(0, length)

// 6,000 Latin characters, no space, no hyphen: the browser has no break point.
const wideEn = fill('Wide0123456789abcdefghijklmnopqrstuvwxyz', EN_LEN)
// 3,000 Han characters and no punctuation.
const wideZh = fill('超长内容不换行也没有标点符号用于检查表格布局是否被撑宽', ZH_LEN)
// '/' + 1,999 characters of one path segment: 2,000 in all.
const widePath = '/api/wide/' + fill('segment0123456789', PATH_LEN - '/api/wide/'.length)

const id = 'wide_layout_fixture'
const phaseName = (i) => `phase_${String(i).padStart(3, '0')}`

const nodes = {}
for (let i = 0; i < PHASES; i++) {
  const last = i === PHASES - 1
  nodes[phaseName(i)] = last
    ? {
        announce: { zh: '测试结束,再见。', en: 'End of the fixture, goodbye.' },
        instruction: { zh: '这是最后一个阶段。', en: 'This is the last phase.' },
        tools: [],
        isTerminal: true,
      }
    : {
        instruction: {
          zh: `第 ${i + 1} 阶段:这是布局测试数据,不要在真实通话中使用。`,
          en: `Phase ${i + 1} of ${PHASES}: layout test data, never for a real call.`,
        },
        tools: ['wide_lookup'],
        transitions: [
          { on: 'TOOL_RESULT', tool: 'wide_lookup', target: phaseName(i + 1) },
        ],
      }
}

const spec = {
  id,
  specVersion: 'v2',
  entry: id,
  initialNode: phaseName(0),
  global: {
    persona: { zh: wideZh, en: wideEn },
    rules: {
      zh: ['这是布局测试数据,不绑定任何号码。'],
      en: ['Layout test data; it is bound to no number.'],
    },
    voice: '',
    closingTarget: phaseName(PHASES - 1),
    maxTurns: 15,
    alwaysAllowedTools: ['transfer_to_agent', 'hangup'],
  },
  nodes,
  tools: {
    wide_lookup: {
      description: {
        zh: '布局测试用的查询,路径超长。',
        en: 'A lookup for layout tests; its path is very long.',
      },
      parameters: { type: 'object', properties: {}, required: [] },
      http: { path: widePath },
    },
  },
}

const text = JSON.stringify(spec, null, 2) + '\n'
const out = process.argv[2]
if (out) writeFileSync(out, text)
else process.stdout.write(text)
