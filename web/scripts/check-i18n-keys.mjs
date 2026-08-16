import fs from 'node:fs'
const zh = JSON.parse(fs.readFileSync('src/locales/zh.json', 'utf8'))
const en = JSON.parse(fs.readFileSync('src/locales/en.json', 'utf8'))
const flat = (o, p = '', r = {}) => {
  for (const k of Object.keys(o)) {
    const v = o[k]
    if (v && typeof v === 'object') flat(v, p + k + '.', r)
    else r[p + k] = 1
  }
  return r
}
const keys = new Set(Object.keys(flat(zh)))
// ops.tsx：ops.stats.* 动态 key（模板串）由 defaultValue 兜底，静态 key 一并校验。
// 正则带标识符边界：排除 setOpt('...') / split(',') 等非 t() 调用的尾缀误匹配；
// 含 ${ 的模板串为动态 key（defaultValue 兜底），跳过。
const files = ['src/pages/dashboard.tsx', 'src/pages/logs.tsx', 'src/pages/user/logs.tsx', 'src/pages/pricing.tsx', 'src/pages/templates.tsx', 'src/pages/accounts.tsx', 'src/pages/groups.tsx', 'src/pages/stats.tsx', 'src/pages/user/stats.tsx', 'src/pages/ops.tsx', 'src/pages/settings.tsx', 'src/pages/user/profile.tsx', 'src/pages/users.tsx']
const re = /(?<![A-Za-z0-9_$])t\(['"`]([^'"`]+)['"`]/g
const problems = []
for (const f of files) {
  const s = fs.readFileSync(f, 'utf8')
  let m
  while ((m = re.exec(s))) {
    const k = m[1]
    if (k.includes('${')) continue
    if (!keys.has(k)) problems.push(f + ': ' + k)
  }
}
// en/zh flat key 全等校验（对齐 caceecd 手动 node 校验做法固化为脚本）：缺失/多余均报错。
const zhFlat = Object.keys(flat(zh))
const enFlat = Object.keys(flat(en))
const zhSet = new Set(zhFlat)
const enSet = new Set(enFlat)
for (const k of zhFlat) if (!enSet.has(k)) problems.push('zh-only key: ' + k)
for (const k of enFlat) if (!zhSet.has(k)) problems.push('en-only key: ' + k)
if (problems.length) {
  console.log(problems.join('\n'))
  process.exitCode = 1
} else {
  console.log('ALL KEYS OK')
}
