import { chromium } from "playwright"

const launch = chromium.launch.bind(chromium)
chromium.launch = async options => {
  const browser = await launch(options)
  const newContext = browser.newContext.bind(browser)
  browser.newContext = async options => {
    const context = await newContext(options)
    const registerRoute = context.route.bind(context)
    context.route = (pattern, handler, options) => registerRoute(pattern, route => {
      const request = route.request()
      const path = new URL(request.url()).pathname
      if (request.method() !== "GET" && /(?:checkout|stripe|plan-changes|plan-assignments|buy-credit|prepaid|auto-recharge)/.test(path)) return route.abort("blockedbyclient")
      return handler(route)
    }, options)
    return context
  }
  return browser
}

await import("./nonpaid-signup.mjs")
