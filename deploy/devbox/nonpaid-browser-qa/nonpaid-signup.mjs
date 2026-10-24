import { chromium } from 'playwright'
import { randomBytes } from 'node:crypto'
import { readFile, mkdir, writeFile, chmod } from 'node:fs/promises'
import { spawnSync } from 'node:child_process'
import path from 'node:path'

const args = Object.fromEntries(process.argv.slice(2).reduce((a, v, i, all) => {
  if (i % 2 === 0) a.push([v.replace(/^--/, ''), all[i + 1]])
  return a
}, []))
const fail = code => { const e = new Error(code); e.qaCode = code; throw e }
const requireValue = key => args[key] || fail(`missing_${key}`)
const sha = key => /^[a-f0-9]{40}$/.test(requireValue(key)) ? args[key] : fail(`invalid_${key}`)
const localURL = (value, key) => {
  const u = new URL(value)
  if (u.protocol !== 'http:' || !['localhost', '127.0.0.1'].includes(u.hostname) || u.username || u.password || u.search || u.hash || u.pathname !== '/') fail(`unsafe_${key}`)
  return u.origin
}
const output = path.resolve(requireValue('output'))
await mkdir(output, { recursive: true, mode: 0o700 })
await chmod(output, 0o700)
const receipt = { schema: 1, scope: 'nonpaid_email_only', started_at: new Date().toISOString(), checks: [], network: [], excluded: ['Google OAuth', 'staff browser approval', 'paid entry', 'activation/release'] }
let browser
let currentStep = 'preflight'
const check = (name, condition, details = {}) => {
  receipt.checks.push({ name, pass: Boolean(condition), ...details })
  if (!condition) fail(name)
}
const step = async (name, action) => {
  currentStep = name
  const start = performance.now()
  await action()
  receipt.checks.push({ name, pass: true, duration_ms: Math.round(performance.now() - start) })
}
const waitUntil = async (fn, code, timeout = 60000) => {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    if (await fn()) return
    await new Promise(r => setTimeout(r, 400))
  }
  fail(code)
}
try {
  check('runner_debug_logging_disabled', !process.env.DEBUG && !process.env.PWDEBUG && !process.env.NODE_DEBUG)
  const consoleOrigin = localURL(args.console || 'http://localhost:3000', 'console')
  const authOrigin = localURL(args.auth || 'http://localhost:54321', 'auth')
  const sinkOrigin = localURL(args.mailpit || 'http://localhost:8025', 'mailpit')
  const backendSha = sha('backend-sha')
  const consoleSha = sha('console-sha')
  const proof = JSON.parse(await readFile(requireValue('runtime-proof'), 'utf8'))
  const expectedLimits = JSON.parse(await readFile(requireValue('free-limits'), 'utf8'))
  const psqlArgv = JSON.parse(await readFile(requireValue('psql-argv'), 'utf8'))
  check('psql_argv_valid', Array.isArray(psqlArgv) && psqlArgv.length > 0 && psqlArgv.every(x => typeof x === 'string') && !psqlArgv.some(x => /password=|postgres(?:ql)?:\/\/.*@/i.test(x)))
  check('runtime_pins_match', proof.backend_sha === backendSha && proof.console_sha === consoleSha)
  check('runtime_observed_recently', Date.now() - Date.parse(proof.observed_at) >= 0 && Date.now() - Date.parse(proof.observed_at) < 1800000)
  check('runtime_released_public_v2', String(proof.phase).toLowerCase() === 'released' && proof.pricing_version === 'v2' && typeof proof.epoch === 'string' && proof.epoch.length > 0)
  check('runtime_isolated_sink', proof.email_sink_only === true && proof.target_devbox === true)
  check('expected_free_limits_valid', expectedLimits && typeof expectedLimits.max_concurrent === 'number' && expectedLimits.max_concurrent > 0)
  receipt.runtime = { backend_sha: backendSha, console_sha: consoleSha, observed_at: proof.observed_at, phase: proof.phase, pricing_version: proof.pricing_version, epoch: proof.epoch }
  const suffix = randomBytes(10).toString('hex')
  const identities = {
    free: { email: `w0-free-${suffix}@example.com`, password: randomBytes(24).toString('base64url') + '!aA1' },
    enterprise: { email: `w0-held-${suffix}@example.com`, password: randomBytes(24).toString('base64url') + '!aA1' },
  }
  receipt.fixture_run = suffix
  const safeRoute = raw => {
    const u = new URL(raw)
    const category = u.origin === consoleOrigin ? 'console' : u.origin === authOrigin ? 'auth' : u.origin === sinkOrigin ? 'mailpit' : 'external'
    const known = /^\/(?:auth\/(?:v1\/(?:token|user|logout|settings|verify|authorize)|signup|signin|callback|pending|auth-code-error)|api\/billing\/plan|sandboxes|_next\/.*)$/
    return { origin: category, path: category !== 'external' && known.test(u.pathname) ? u.pathname.replace(/\/_next\/.*/, '/_next/:asset') : ':other' }
  }
  browser = await chromium.launch({ headless: true })
  const contexts = []
  const newPage = async label => {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, serviceWorkers: 'block' })
    contexts.push(context)
    await context.route('**/*', route => {
      const req = route.request()
      const u = new URL(req.url())
      if (!['http:', 'https:'].includes(u.protocol)) return route.continue()
      if (![consoleOrigin, authOrigin, sinkOrigin].includes(u.origin)) return route.abort('blockedbyclient')
      if (req.method() !== 'GET' && /(?:checkout|stripe|plan-changes|plan-assignments|buy-credit)/.test(u.pathname)) return route.abort('blockedbyclient')
      return route.continue()
    })
    const page = await context.newPage()
    page.setDefaultTimeout(20000)
    page.on('response', response => {
      if (response.status() >= 400) receipt.network.push({ case: label, ...safeRoute(response.url()), status: response.status() })
    })
    page.on('requestfailed', request => receipt.network.push({ case: label, ...safeRoute(request.url()), failed: true, navigation_cancelled: request.failure()?.errorText === 'net::ERR_ABORTED' }))
    return page
  }
  const screenshot = async (page, label) => {
    await page.screenshot({ path: path.join(output, `${label}.png`), animations: 'disabled', mask: [page.locator('input,textarea,iframe'), ...Object.values(identities).map(v => page.getByText(v.email, { exact: false }))] })
  }
  const messages = async email => {
    const response = await fetch(`${sinkOrigin}/api/v1/search?query=${encodeURIComponent('to:' + email)}`, { signal: AbortSignal.timeout(5000) })
    if (!response.ok) fail('mailpit_search_unavailable')
    const data = await response.json()
    return (data.messages || []).filter(m => (m.To || []).some(t => t.Address?.toLowerCase() === email))
  }
  const confirmationLink = async email => {
    let result
    await waitUntil(async () => {
      const candidates = await messages(email)
      for (const message of candidates) {
        const response = await fetch(`${sinkOrigin}/api/v1/message/${encodeURIComponent(message.ID)}`, { signal: AbortSignal.timeout(5000) })
        if (!response.ok) fail('mailpit_message_unavailable')
        const content = await response.json()
        const html = String(content.HTML || '')
        const hrefs = [...html.matchAll(/href=["']([^"']+)["']/g)].map(m => m[1].replaceAll('&amp;', '&').replaceAll('&#38;', '&'))
        for (const raw of hrefs) {
          let u
          try { u = new URL(raw) } catch { continue }
          if (u.origin === consoleOrigin && u.pathname === '/auth/callback' && u.searchParams.has('token_hash')) { result = u.href; return true }
        }
      }
      return false
    }, 'confirmation_email_not_received')
    return result
  }
  const signup = async kind => {
    const page = await newPage(kind)
    const identity = identities[kind]
    await page.goto(`${consoleOrigin}/auth/signup?offer=${kind === 'free' ? 'free' : 'enterprise'}`, { waitUntil: 'domcontentloaded' })
    await page.getByPlaceholder('Full Name', { exact: true }).fill('Example QA')
    await page.getByPlaceholder('Email', { exact: true }).fill(identity.email)
    await page.getByPlaceholder('Password', { exact: true }).fill(identity.password)
    await page.getByPlaceholder('Confirm Password', { exact: true }).fill(identity.password)
    await page.getByRole('button', { name: 'Sign Up', exact: true }).click()
    await page.getByRole('heading', { name: 'Check Your Email', exact: true }).waitFor()
    await screenshot(page, `${kind}-email-sent`)
    return page
  }
  const confirm = async (page, kind) => {
    const link = await confirmationLink(identities[kind].email)
    let seen = false
    page.on('request', req => { const u = new URL(req.url()); if (u.origin === consoleOrigin && u.pathname === '/auth/callback') seen = true })
    await page.goto(link, { waitUntil: 'domcontentloaded' })
    await waitUntil(async () => new URL(page.url()).pathname === (kind === 'free' ? '/sandboxes' : '/auth/pending'), `${kind}_callback_destination`)
    check(`${kind}_callback_exercised`, seen)
    if (kind === 'enterprise') await page.getByRole('heading', { name: /set up your Enterprise plan/ }).waitFor()
    else await page.getByRole('heading', { name: /sandboxes/i }).first().waitFor()
    await screenshot(page, `${kind}-confirmed`)
  }
  const login = async kind => {
    const page = await newPage(`${kind}-login`)
    await page.goto(`${consoleOrigin}/auth/signin`, { waitUntil: 'domcontentloaded' })
    await page.getByPlaceholder('Email', { exact: true }).fill(identities[kind].email)
    await page.getByPlaceholder('Password', { exact: true }).fill(identities[kind].password)
    await page.getByRole('button', { name: 'Sign In', exact: true }).click()
    await waitUntil(async () => new URL(page.url()).pathname === (kind === 'free' ? '/sandboxes' : '/auth/pending'), `${kind}_login_destination`)
    return page
  }
  let freePage
  await step('free_email_signup_and_sink_confirmation', async () => { freePage = await signup('free'); await confirm(freePage, 'free') })
  await step('free_fresh_context_password_login_and_plan_limits', async () => {
    freePage = await login('free')
    const response = await freePage.context().request.get(`${consoleOrigin}/api/billing/plan`, { maxRedirects: 0 })
    check('free_plan_http_200', response.status() === 200, { status: response.status() })
    const body = await response.json()
    check('free_plan_authoritative', body.plan?.kind === 'free' && body.plan?.plan_key === 'free')
    check('free_effective_limits_match', Object.entries(expectedLimits).every(([key, value]) => body.plan?.limits?.[key] === value))
    receipt.free_effective_limits = Object.fromEntries(Object.keys(expectedLimits).map(key => [key, body.plan.limits[key]]))
    await screenshot(freePage, 'free-password-login')
  })
  await step('enterprise_email_confirm_remains_held', async () => {
    const page = await signup('enterprise')
    await confirm(page, 'enterprise')
    await page.goto(`${consoleOrigin}/sandboxes`, { waitUntil: 'domcontentloaded' })
    await waitUntil(async () => ['/auth/pending', '/auth/signin'].includes(new URL(page.url()).pathname), 'enterprise_dashboard_forbidden')
    const response = await page.context().request.get(`${consoleOrigin}/api/billing/plan`, { maxRedirects: 0 })
    check('enterprise_plan_access_denied', [401, 403, 307, 302, 303].includes(response.status()), { status: response.status() })
    const logged = await login('enterprise')
    await logged.getByRole('heading', { name: /set up your Enterprise plan/ }).waitFor()
    await screenshot(logged, 'enterprise-password-login-held')
    const mail = await messages(identities.enterprise.email)
    check('enterprise_no_approval_email', !mail.some(m => /approved|account is ready|access.*ready/i.test(String(m.Subject || ''))))
  })
  await step('readonly_durable_assignment_and_hold_proof', async () => {
    const literal = value => "'" + value.replaceAll("'", "''") + "'"
    const sql = `BEGIN READ ONLY; SET LOCAL statement_timeout='10s'; SELECT json_build_object(
      'free_users',(SELECT count(*) FROM auth.users WHERE email=${literal(identities.free.email)} AND email_confirmed_at IS NOT NULL),
      'free_teams',(SELECT count(DISTINCT tm.team_id) FROM team_member tm JOIN auth.users u ON u.id=tm.profile_id WHERE u.email=${literal(identities.free.email)} AND tm.role='owner'),
      'free_assignments',(SELECT count(*) FROM team_plan_assignment a JOIN team_member tm ON tm.team_id=a.team_id JOIN auth.users u ON u.id=tm.profile_id WHERE u.email=${literal(identities.free.email)} AND tm.role='owner' AND a.effective_to IS NULL AND a.kind='free' AND a.source='signup'),
      'held_users',(SELECT count(*) FROM auth.users WHERE email=${literal(identities.enterprise.email)} AND app_metadata_placeholder),
      'held_teams',(SELECT count(*) FROM team_member tm JOIN auth.users u ON u.id=tm.profile_id WHERE u.email=${literal(identities.enterprise.email)})
    ); ROLLBACK;`.replace('app_metadata_placeholder', "raw_app_meta_data->>'signup_review'='pending'")
    const result = spawnSync(psqlArgv[0], psqlArgv.slice(1), { input: sql, encoding: 'utf8', timeout: 30000, maxBuffer: 1024 * 1024 })
    check('readonly_db_command_success', result.status === 0)
    let counts
    try { counts = JSON.parse(result.stdout.trim()) } catch { fail('readonly_db_invalid_sanitized_counts') }
    check('durable_free_once_and_enterprise_held', counts.free_users === 1 && counts.free_teams === 1 && counts.free_assignments === 1 && counts.held_users === 1 && counts.held_teams === 0)
    receipt.database_counts = counts
  })
  const material = receipt.network.filter(n => n.origin !== 'external' && (n.status >= 500 || (n.failed && !n.navigation_cancelled)))
  check('no_internal_network_failures_or_5xx', material.length === 0, { count: material.length })
  receipt.result = 'PASS'
} catch (error) {
  receipt.result = 'FAIL'
  receipt.failure = { step: currentStep, code: error.qaCode || 'browser_or_runner_failure', error_class: ['TimeoutError', 'SyntaxError'].includes(error.name) ? error.name : 'Error' }
} finally {
  if (browser) await browser.close().catch(() => {})
  receipt.finished_at = new Date().toISOString()
  await writeFile(path.join(output, 'receipt.json'), JSON.stringify(receipt, null, 2) + '\n', { mode: 0o600 })
  process.stdout.write(JSON.stringify({ result: receipt.result, step: receipt.failure?.step, code: receipt.failure?.code, receipt: path.join(output, 'receipt.json') }) + '\n')
  process.exitCode = receipt.result === 'PASS' ? 0 : 1
}
