package utils

import "context"

// automationFlagScript is injected into every document a page of the agent loads,
// before the page's own scripts run. A browser that a protocol client drives
// answers navigator.webdriver with true — a headful launch that is given nothing
// but the DevTools port reports it, measured on Chrome 153 — and that single
// boolean is the first thing an anti-bot script reads. The page is told otherwise
// here.
//
// The script is written to leave as little of itself behind as it can:
//
//   - It does nothing at all when the browser already answers false (a tab of the
//     user's own browser that no launch of ours touched). A browser that was not
//     patched cannot be caught being patched.
//   - It replaces the accessor where Chrome defines it (Navigator.prototype),
//     keeps the shape of the native descriptor (enumerable, configurable, no
//     setter) and keeps the name of the native getter, so
//     navigator.webdriver stays a property that is reported false rather than a
//     property that went missing.
//   - It gives the replacement the source text of the native getter through a
//     trap on Function.prototype.toString, which answers for the replacement (and
//     for itself) and passes every other function through. Without it, a detector
//     reading the getter's source would see a script instead of "[native code]".
//
// The switch that does this natively, --disable-blink-features=AutomationControlled,
// is not usable: Chrome answers it with an "unsupported command-line flag" bar
// across the window, which is a sign of automation in itself (see
// browserLaunchArgs).
const automationFlagScript = `(() => {
	const proto = Navigator.prototype;
	const descriptor = Object.getOwnPropertyDescriptor(proto, 'webdriver');
	if (!descriptor || typeof descriptor.get !== 'function') return;
	if (descriptor.get.call(navigator) !== true) return;
	const nativeGetter = descriptor.get;
	const nativeToString = Function.prototype.toString;
	const getterSource = nativeToString.call(nativeGetter);
	const toStringSource = nativeToString.call(nativeToString);
	const getter = function () { return false; };
	Object.defineProperty(getter, 'name', { value: nativeGetter.name, configurable: true });
	Object.defineProperty(proto, 'webdriver', {
		get: getter,
		set: descriptor.set,
		enumerable: descriptor.enumerable,
		configurable: descriptor.configurable,
	});
	let patched;
	patched = new Proxy(nativeToString, {
		apply(target, thisArg, args) {
			if (thisArg === getter) return getterSource;
			if (thisArg === patched) return toStringSource;
			return Reflect.apply(target, thisArg, args);
		},
	});
	Function.prototype.toString = patched;
})();`

// hideAutomation registers the script that keeps a page from reading the
// automation flag of the browser (see automationFlagScript). It runs on every
// document the page loads from now on, before the document's own scripts, so it
// has to be registered before the navigation that loads the page — the document
// that is already there is out of its reach, which is why a tab the user has open
// is left as it is.
func (p *Page) hideAutomation(ctx context.Context) error {
	params := map[string]any{"source": automationFlagScript}
	return p.call(ctx, "Page.addScriptToEvaluateOnNewDocument", params, nil)
}
