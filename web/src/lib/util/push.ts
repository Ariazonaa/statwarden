// Push monitors are fed by the monitored job calling a URL that carries a
// secret token. The server keeps only a hash of that token, so it can be shown
// exactly once: in the response that minted it. These helpers build what the
// UI shows at that moment and carry a token from the create form to the detail
// page, where it is displayed.

/** The heartbeat URL for a token, on the origin the browser is using. */
export function pushUrl(token: string, origin: string): string {
	return `${origin.replace(/\/+$/, '')}/api/v1/push/${token}`;
}

/**
 * A ready-to-paste example for a cron job or script. The route accepts POST
 * only; -f makes curl fail on an HTTP error, -m bounds a hanging request.
 */
export function pushCurl(url: string): string {
	return `curl -fsS -m 10 -X POST "${url}?status=up&msg=OK"`;
}

// One pending token per monitor, in memory only: never persisted, gone on
// reload, and handed out once.
const pending = new Map<string, string>();

/** Remembers a freshly minted token for the monitor's detail page. */
export function rememberPushToken(monitorId: string, token: string): void {
	pending.set(monitorId, token);
}

/** Returns the remembered token for a monitor once, then forgets it. */
export function takePushToken(monitorId: string): string | undefined {
	const tok = pending.get(monitorId);
	pending.delete(monitorId);
	return tok;
}
