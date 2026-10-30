/**
 * Pins the form <-> wire conversion in ./registry.ts, because the shapes it
 * produces are the ones Go's json.Unmarshal and Validate() see. A regression
 * here is not a UI bug — it is a 400 on every save of the affected type.
 *
 * This cannot check the KEYS against the Go structs (that would need the Go
 * source); it checks that each declared `shape` produces the JSON its config
 * struct decodes into.
 */
import { describe, expect, it } from 'vitest';
import {
	configFromForm,
	configToForm,
	isFieldVisible,
	monitorTypeSpecs,
	notificationTypeSpecs
} from './registry';
import type { FieldSpec } from './types';

const specFor = (type: string) => {
	const s =
		monitorTypeSpecs.find((x) => x.type === type) ??
		notificationTypeSpecs.find((x) => x.type === type);
	if (!s) throw new Error(`no spec for ${type}`);
	return s;
};

describe('configFromForm', () => {
	it('parses accepted_status_codes into the array StatusCodeSet accepts', () => {
		const out = configFromForm(specFor('http').fields, {
			url: 'https://example.com',
			accepted_status_codes: '200-299, 301, 418'
		});
		// Single codes as numbers, ranges as "lo-hi" strings — exactly what
		// StatusCodeSet.UnmarshalJSON reads and MarshalJSON writes back.
		expect(out.accepted_status_codes).toEqual(['200-299', 301, 418]);
	});

	it('parses headers into a string map', () => {
		const out = configFromForm(specFor('http').fields, {
			url: 'https://example.com',
			headers: 'X-Api-Key: secret\nAccept: application/json'
		});
		expect(out.headers).toEqual({ 'X-Api-Key': 'secret', Accept: 'application/json' });
	});

	it('keeps colons inside a header value', () => {
		const out = configFromForm(specFor('http').fields, {
			url: 'https://x.test',
			headers: 'Authorization: Bearer a:b:c'
		});
		expect(out.headers).toEqual({ Authorization: 'Bearer a:b:c' });
	});

	it('parses list fields into []string, splitting on newlines and commas', () => {
		const out = configFromForm(specFor('smtp').fields, {
			host: 'smtp.test',
			from: 'a@test',
			to: 'a@test\nb@test, c@test'
		});
		expect(out.to).toEqual(['a@test', 'b@test', 'c@test']);
	});

	it('parses a webpush subscription into a nested object, not a string', () => {
		const out = configFromForm(specFor('webpush').fields, {
			vapid_public_key: 'pub',
			vapid_private_key: 'priv',
			subject: 'mailto:ops@test',
			subscription: '{"endpoint":"https://push.test/x","keys":{"p256dh":"p","auth":"a"}}'
		});
		expect(out.subscription).toEqual({
			endpoint: 'https://push.test/x',
			keys: { p256dh: 'p', auth: 'a' }
		});
	});

	it('drops empty strings so an optional int never arrives as ""', () => {
		const out = configFromForm(specFor('postgres').fields, {
			host: 'db.test',
			port: '',
			query: ''
		});
		expect(out).not.toHaveProperty('port');
		expect(out).not.toHaveProperty('query');
		expect(out.host).toBe('db.test');
	});

	it('keeps false — an explicit follow_redirects:false is not an absent one', () => {
		const out = configFromForm(specFor('http').fields, {
			url: 'https://x.test',
			follow_redirects: false
		});
		expect(out.follow_redirects).toBe(false);
	});

	it('drops keys left behind by a previous type selection', () => {
		const out = configFromForm(specFor('http').fields, {
			url: 'https://x.test',
			oid: '1.3.6.1', // typed while the type was still snmp
			community: 'public'
		});
		expect(out).toEqual({ url: 'https://x.test' });
	});

	it('sends numbers as numbers', () => {
		const out = configFromForm(specFor('tcp').fields, { host: 'db.test', port: '5432' });
		expect(out.port).toBe(5432);
	});

	it('omits fields hidden by an unsatisfied showIf so a stale secret never ships', () => {
		// The user picked Basic auth, typed a credential, then switched auth back to
		// none. showIf only hides the widgets — their values linger in form state —
		// so without dropping them the stale secret is serialised and the server's
		// Validate() rejects the now-inconsistent config with a 400.
		const out = configFromForm(specFor('http').fields, {
			url: 'https://x.test',
			auth_method: 'none',
			basic_username: 'admin',
			basic_password: 'hunter2',
			bearer_token: 'stale-token'
		});
		expect(out).not.toHaveProperty('basic_username');
		expect(out).not.toHaveProperty('basic_password');
		expect(out).not.toHaveProperty('bearer_token');
		expect(out).toEqual({ url: 'https://x.test', auth_method: 'none' });
	});

	it('keeps a shown showIf field once its condition is satisfied', () => {
		const out = configFromForm(specFor('http').fields, {
			url: 'https://x.test',
			auth_method: 'bearer',
			bearer_token: 'live-token'
		});
		expect(out.bearer_token).toBe('live-token');
	});
});

describe('configToForm', () => {
	it('round-trips every shaped field back to the same wire value', () => {
		const fields = specFor('http').fields;
		const wire = {
			url: 'https://example.com',
			accepted_status_codes: ['200-299', 301],
			headers: { 'X-Api-Key': 'secret' }
		};
		expect(configFromForm(fields, configToForm(fields, wire))).toEqual(wire);
	});

	it('leaves unknown keys alone so an edit cannot destroy them', () => {
		const out = configToForm(specFor('http').fields, { url: 'https://x.test', future_field: 1 });
		expect(out.future_field).toBe(1);
	});
});

describe('isFieldVisible', () => {
	const snmp = specFor('snmp').fields;
	const field = (key: string) => snmp.find((f) => f.key === key) as FieldSpec;

	it('shows community only for v2c and username only for v3', () => {
		expect(isFieldVisible(field('community'), { version: '2c' })).toBe(true);
		expect(isFieldVisible(field('community'), { version: '3' })).toBe(false);
		expect(isFieldVisible(field('username'), { version: '3' })).toBe(true);
	});

	it('shows auth_protocol for both levels that require it', () => {
		expect(isFieldVisible(field('auth_protocol'), { security_level: 'authNoPriv' })).toBe(true);
		expect(isFieldVisible(field('auth_protocol'), { security_level: 'authPriv' })).toBe(true);
		expect(isFieldVisible(field('auth_protocol'), { security_level: 'noAuthNoPriv' })).toBe(false);
	});
});

describe('spec hygiene', () => {
	const all = [...monitorTypeSpecs, ...notificationTypeSpecs];

	it('uses snake_case wire keys everywhere — a camelCase key is an invented one', () => {
		for (const spec of all) {
			for (const f of spec.fields) {
				expect(f.key, `${spec.type}.${f.key}`).toMatch(/^[a-z0-9]+(_[a-z0-9]+)*$/);
			}
		}
	});

	it('has no duplicate field keys within a type', () => {
		for (const spec of all) {
			const keys = spec.fields.map((f) => f.key);
			expect(new Set(keys).size, spec.type).toBe(keys.length);
		}
	});

	it('points every showIf at a field the same type actually declares', () => {
		for (const spec of all) {
			const keys = new Set(spec.fields.map((f) => f.key));
			for (const f of spec.fields) {
				if (f.showIf) expect(keys.has(f.showIf.key), `${spec.type}.${f.key}`).toBe(true);
			}
		}
	});
});
