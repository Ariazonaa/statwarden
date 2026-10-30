/**
 * Per-type config form schemas for monitors and notification channels.
 *
 * These specs are AUTHORED HERE, not served. The server's
 * GET /api/v1/registry/monitor-types returns only `{type, passive}` and
 * /registry/notification-types only `{type}` — neither carries per-field
 * metadata, so the dynamic form cannot be driven from the API alone. What the
 * registry endpoints are good for is discovering which types a given build has
 * compiled in; the field schema for those types lives in this file.
 *
 * Because the schema is authored client-side, every `key` below MUST be the
 * exact json tag of the corresponding field in the Go config struct
 * (internal/monitor/types/<x>check.Config and
 * internal/notify/providers/<x>.Config), and every field's produced JSON must
 * satisfy that struct's Validate(). A key that is merely plausible — `webhookUrl`
 * for `webhook_url`, or a comma-joined string where StatusCodeSet wants an
 * array — is not a cosmetic problem: the create/edit request fails with a 400
 * that the form surfaces as a generic save error.
 *
 * There is currently NO automated check tying this file to the Go structs, so
 * the two drift silently. Adding a field to a Config struct means editing this
 * file by hand in the same change.
 *
 * `configFromForm` / `configToForm` at the bottom translate between what the
 * widgets hold (always scalars — a text input cannot hold an array) and the
 * shapes Go decodes into. Nothing should read `config` straight out of the form
 * state and post it.
 */
import type { FieldOption, FieldSpec, MonitorTypeSpec, NotificationTypeSpec } from './types';

// ---- Shared field fragments ------------------------------------------------
// Many checkers repeat the same field with the same json tag and the same
// meaning; defining them once keeps the key/label pairing from drifting between
// copies. `port` and `connectTimeout` take the checker's own default, which
// differs per type (5432/3306/27017/…), so they are functions.

const hostField: FieldSpec = {
	key: 'host',
	label: 'configField.host',
	type: 'text',
	required: true,
	placeholder: 'db.internal'
};

const portField = (dflt: number, required = false): FieldSpec => ({
	key: 'port',
	label: 'configField.port',
	type: 'number',
	default: dflt,
	required,
	min: required ? 1 : 0,
	max: 65535
});

const connectTimeoutField = (dflt = 10): FieldSpec => ({
	key: 'connect_timeout_seconds',
	label: 'configField.connectTimeout',
	type: 'number',
	default: dflt,
	min: 0
});

const ignoreTlsField: FieldSpec = {
	key: 'ignore_tls_errors',
	label: 'configField.ignoreTlsErrors',
	type: 'boolean',
	default: false
};

const passwordField: FieldSpec = {
	key: 'password',
	label: 'configField.password',
	type: 'password'
};

/** Turns wire tokens into options that must NOT be translated. */
const tokens = (...values: string[]): FieldOption[] => values.map((v) => ({ value: v, label: v }));

// ---- Monitor types ---------------------------------------------------------

export const monitorTypeSpecs: MonitorTypeSpec[] = [
	{
		type: 'http',
		label: 'monitorType.http',
		family: 'http',
		description: 'monitorType.http.desc',
		fields: [
			{
				key: 'url',
				label: 'configField.url',
				type: 'url',
				placeholder: 'https://example.com',
				required: true
			},
			{
				key: 'method',
				label: 'configField.method',
				type: 'select',
				default: 'GET',
				// Uppercase is mandatory: Validate compares against http.MethodGet
				// and friends verbatim, so "get" is rejected.
				options: tokens('GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS')
			},
			{
				key: 'accepted_status_codes',
				label: 'configField.acceptedStatus',
				type: 'text',
				shape: 'statuscodes',
				placeholder: '200-299, 301',
				hint: 'configField.acceptedStatus.hint'
			},
			{
				key: 'headers',
				label: 'configField.headers',
				type: 'textarea',
				shape: 'keyvalue',
				placeholder: 'X-Api-Key: secret',
				hint: 'configField.headers.hint'
			},
			{ key: 'user_agent', label: 'configField.userAgent', type: 'text' },
			{ key: 'body', label: 'configField.body', type: 'textarea' },
			{
				key: 'body_encoding',
				label: 'configField.bodyEncoding',
				type: 'select',
				default: 'raw',
				options: [
					{ value: 'raw', label: 'raw', labelKey: 'configOption.bodyRaw' },
					{ value: 'json', label: 'JSON' },
					{ value: 'form', label: 'form', labelKey: 'configOption.bodyForm' }
				],
				hint: 'configField.bodyEncoding.hint'
			},
			{
				key: 'auth_method',
				label: 'configField.authMethod',
				type: 'select',
				default: 'none',
				options: [
					{ value: 'none', label: 'none', labelKey: 'configOption.authNone' },
					{ value: 'basic', label: 'Basic' },
					{ value: 'bearer', label: 'Bearer' }
				]
			},
			{
				key: 'basic_username',
				label: 'configField.username',
				type: 'text',
				required: true,
				showIf: { key: 'auth_method', equals: 'basic' }
			},
			{
				key: 'basic_password',
				label: 'configField.password',
				type: 'password',
				showIf: { key: 'auth_method', equals: 'basic' }
			},
			{
				key: 'bearer_token',
				label: 'configField.bearerToken',
				type: 'password',
				required: true,
				showIf: { key: 'auth_method', equals: 'bearer' }
			},
			{ key: 'tls_client_cert_pem', label: 'configField.clientCert', type: 'textarea' },
			{ key: 'tls_client_key_pem', label: 'configField.clientKey', type: 'textarea' },
			{
				key: 'follow_redirects',
				label: 'configField.followRedirects',
				type: 'boolean',
				default: true
			},
			{
				key: 'max_redirects',
				label: 'configField.maxRedirects',
				type: 'number',
				default: 10,
				min: 0
			},
			ignoreTlsField,
			{
				key: 'proxy_url',
				label: 'configField.proxyUrl',
				type: 'text',
				placeholder: 'http://proxy.internal:3128',
				hint: 'configField.proxyUrl.hint'
			},
			{ key: 'keyword', label: 'configField.keyword', type: 'text', placeholder: 'healthy' },
			{ key: 'keyword_absent', label: 'configField.keywordAbsent', type: 'text' },
			{
				key: 'json_path',
				label: 'configField.jsonPath',
				type: 'text',
				placeholder: 'checks.0.state',
				hint: 'configField.jsonPath.hint'
			},
			{ key: 'json_expected', label: 'configField.jsonExpected', type: 'text' },
			{ key: 'header_name', label: 'configField.headerName', type: 'text' },
			{
				key: 'header_value',
				label: 'configField.headerValue',
				type: 'text',
				hint: 'configField.headerValue.hint'
			},
			{ key: 'header_value_regex', label: 'configField.headerValueRegex', type: 'text' },
			{
				key: 'max_response_time_ms',
				label: 'configField.maxResponseTime',
				type: 'number',
				min: 0,
				hint: 'configField.maxResponseTime.hint'
			}
		]
	},
	{
		type: 'websocket',
		label: 'monitorType.websocket',
		family: 'http',
		description: 'monitorType.websocket.desc',
		fields: [
			{
				key: 'url',
				label: 'configField.url',
				type: 'text',
				placeholder: 'wss://example.com/socket',
				required: true,
				hint: 'configField.wsUrl.hint'
			},
			{
				key: 'headers',
				label: 'configField.headers',
				type: 'textarea',
				shape: 'keyvalue',
				placeholder: 'Authorization: Bearer token',
				hint: 'configField.headers.hint'
			},
			ignoreTlsField
		]
	},
	{
		type: 'tcp',
		label: 'monitorType.tcp',
		family: 'network',
		description: 'monitorType.tcp.desc',
		// No default port: Validate demands 1-65535, so an absent port is a 400.
		fields: [hostField, { ...portField(0, true), default: undefined, placeholder: '5432' }]
	},
	{
		type: 'udp',
		label: 'monitorType.udp',
		family: 'network',
		description: 'monitorType.udp.desc',
		// No default port: Validate demands 1-65535, so an absent port is a 400.
		fields: [
			{ ...hostField, placeholder: 'syslog.internal' },
			{ ...portField(0, true), default: undefined, placeholder: '514' },
			{
				key: 'payload_encoding',
				label: 'configField.payloadEncoding',
				type: 'select',
				default: 'text',
				options: tokens('text', 'hex', 'base64'),
				hint: 'configField.payloadEncoding.hint'
			},
			{ key: 'payload', label: 'configField.payload', type: 'textarea' },
			{
				key: 'expect_response',
				label: 'configField.expectResponse',
				type: 'boolean',
				default: true,
				hint: 'configField.expectResponse.hint'
			},
			{
				key: 'expect',
				label: 'configField.expectedPayload',
				type: 'text',
				// Go rejects the pair outright: an assertion on a reply the
				// check is told not to wait for could only ever fail.
				showIf: { key: 'expect_response', equals: true }
			},
			{
				key: 'response_timeout_seconds',
				label: 'configField.responseTimeout',
				type: 'number',
				default: 5,
				min: 0
			}
		]
	},
	{
		type: 'ntp',
		label: 'monitorType.ntp',
		family: 'network',
		description: 'monitorType.ntp.desc',
		fields: [
			{ ...hostField, placeholder: 'ptbtime1.ptb.de' },
			portField(123),
			{
				key: 'max_offset_ms',
				label: 'configField.maxOffsetMs',
				type: 'number',
				default: 1000,
				min: 1,
				hint: 'configField.maxOffsetMs.hint'
			},
			{
				key: 'max_stratum',
				label: 'configField.maxStratum',
				type: 'number',
				min: 0,
				max: 15,
				hint: 'configField.maxStratum.hint'
			}
		]
	},
	{
		type: 'ping',
		label: 'monitorType.ping',
		family: 'network',
		description: 'monitorType.ping.desc',
		fields: [
			{ ...hostField, placeholder: '1.1.1.1' },
			{ key: 'count', label: 'configField.count', type: 'number', default: 4, min: 0, max: 100 },
			{
				key: 'packet_loss_threshold_percent',
				label: 'configField.lossThreshold',
				type: 'number',
				default: 100,
				min: 0,
				max: 100,
				hint: 'configField.lossThreshold.hint'
			}
		]
	},
	{
		type: 'dns',
		label: 'monitorType.dns',
		family: 'network',
		description: 'monitorType.dns.desc',
		fields: [
			{ ...hostField, placeholder: 'example.com' },
			{
				key: 'record_type',
				label: 'configField.recordType',
				type: 'select',
				default: 'A',
				options: tokens('A', 'AAAA', 'CNAME', 'MX', 'TXT', 'NS', 'SOA', 'SRV', 'PTR', 'CAA')
			},
			{
				key: 'resolver',
				label: 'configField.resolver',
				type: 'text',
				placeholder: '1.1.1.1:53',
				hint: 'configField.resolver.hint'
			},
			{
				key: 'expected',
				label: 'configField.expectedRecords',
				type: 'textarea',
				shape: 'stringlist',
				hint: 'configField.list.hint'
			}
		]
	},
	{
		type: 'cert',
		label: 'monitorType.cert',
		family: 'network',
		description: 'monitorType.cert.desc',
		fields: [
			{ ...hostField, placeholder: 'example.com' },
			portField(443),
			{
				key: 'server_name',
				label: 'configField.serverName',
				type: 'text',
				hint: 'configField.serverName.hint'
			},
			{
				key: 'warn_days',
				label: 'configField.warnDays',
				type: 'number',
				default: 14,
				min: 0,
				hint: 'configField.warnDays.hint'
			},
			ignoreTlsField
		]
	},
	{
		type: 'snmp',
		label: 'monitorType.snmp',
		family: 'network',
		description: 'monitorType.snmp.desc',
		fields: [
			hostField,
			portField(161),
			{
				key: 'version',
				label: 'configField.snmpVersion',
				type: 'select',
				default: '2c',
				options: tokens('2c', '3')
			},
			{
				key: 'oid',
				label: 'configField.oid',
				type: 'text',
				required: true,
				placeholder: '1.3.6.1.2.1.1.3.0'
			},
			{
				key: 'community',
				label: 'configField.community',
				type: 'password',
				required: true,
				showIf: { key: 'version', equals: '2c' }
			},
			{
				key: 'username',
				label: 'configField.username',
				type: 'text',
				required: true,
				showIf: { key: 'version', equals: '3' }
			},
			{
				key: 'security_level',
				label: 'configField.securityLevel',
				type: 'select',
				required: true,
				showIf: { key: 'version', equals: '3' },
				options: tokens('noAuthNoPriv', 'authNoPriv', 'authPriv')
			},
			{
				key: 'auth_protocol',
				label: 'configField.authProtocol',
				type: 'select',
				required: true,
				showIf: { key: 'security_level', in: ['authNoPriv', 'authPriv'] },
				options: tokens('MD5', 'SHA', 'SHA224', 'SHA256', 'SHA384', 'SHA512')
			},
			{
				key: 'auth_password',
				label: 'configField.authPassword',
				type: 'password',
				required: true,
				showIf: { key: 'security_level', in: ['authNoPriv', 'authPriv'] }
			},
			{
				key: 'priv_protocol',
				label: 'configField.privProtocol',
				type: 'select',
				required: true,
				showIf: { key: 'security_level', equals: 'authPriv' },
				options: tokens('DES', 'AES', 'AES192', 'AES256')
			},
			{
				key: 'priv_password',
				label: 'configField.privPassword',
				type: 'password',
				required: true,
				showIf: { key: 'security_level', equals: 'authPriv' }
			},
			{
				key: 'context_name',
				label: 'configField.contextName',
				type: 'text',
				showIf: { key: 'version', equals: '3' }
			},
			{
				key: 'expected_value',
				label: 'configField.expectedValue',
				type: 'text',
				hint: 'configField.expectedValue.hint'
			},
			{
				key: 'operator',
				label: 'configField.operator',
				type: 'select',
				default: '',
				options: [
					{ value: '', label: '—', labelKey: 'configOption.noComparison' },
					...tokens('==', '!=', '>', '>=', '<', '<=')
				]
			},
			{
				key: 'threshold',
				label: 'configField.threshold',
				type: 'number',
				hint: 'configField.threshold.hint'
			}
		]
	},
	{
		type: 'radius',
		label: 'monitorType.radius',
		family: 'network',
		description: 'monitorType.radius.desc',
		fields: [
			hostField,
			portField(1812),
			{ key: 'secret', label: 'configField.sharedSecret', type: 'password', required: true },
			{ key: 'username', label: 'configField.username', type: 'text', required: true },
			passwordField,
			{ key: 'nas_identifier', label: 'configField.nasIdentifier', type: 'text' }
		]
	},
	{
		type: 'postgres',
		label: 'monitorType.postgres',
		family: 'database',
		description: 'monitorType.postgres.desc',
		fields: [
			hostField,
			portField(5432),
			{ key: 'user', label: 'configField.user', type: 'text' },
			passwordField,
			{ key: 'database', label: 'configField.database', type: 'text' },
			{
				key: 'ssl_mode',
				label: 'configField.sslMode',
				type: 'select',
				default: 'prefer',
				options: tokens('disable', 'allow', 'prefer', 'require', 'verify-ca', 'verify-full')
			},
			connectTimeoutField(),
			{
				key: 'query',
				label: 'configField.query',
				type: 'text',
				placeholder: 'SELECT 1',
				hint: 'configField.query.hint'
			},
			{ key: 'expected_result', label: 'configField.expectedResult', type: 'text' }
		]
	},
	{
		type: 'mysql',
		label: 'monitorType.mysql',
		family: 'database',
		description: 'monitorType.mysql.desc',
		fields: [
			hostField,
			portField(3306),
			{ key: 'user', label: 'configField.user', type: 'text' },
			passwordField,
			{ key: 'database', label: 'configField.database', type: 'text' },
			{
				// A STRING enum, not a bool — the go-sql-driver tls parameter. A JSON
				// boolean here is a decode error, hence a select over string tokens.
				key: 'tls',
				label: 'configField.tlsMode',
				type: 'select',
				default: 'false',
				options: tokens('false', 'true', 'skip-verify', 'preferred')
			},
			connectTimeoutField(),
			{
				key: 'query',
				label: 'configField.query',
				type: 'text',
				placeholder: 'SELECT 1',
				hint: 'configField.query.hint'
			},
			{ key: 'expected_result', label: 'configField.expectedResult', type: 'text' }
		]
	},
	{
		type: 'mongodb',
		label: 'monitorType.mongodb',
		family: 'database',
		description: 'monitorType.mongodb.desc',
		fields: [
			hostField,
			portField(27017),
			{ key: 'user', label: 'configField.user', type: 'text' },
			passwordField,
			{ key: 'database', label: 'configField.database', type: 'text' },
			{ key: 'tls', label: 'configField.tls', type: 'boolean', default: false },
			{
				key: 'tls_skip_verify',
				label: 'configField.tlsSkipVerify',
				type: 'boolean',
				default: false,
				showIf: { key: 'tls', equals: true }
			},
			connectTimeoutField()
		]
	},
	{
		type: 'redis',
		label: 'monitorType.redis',
		family: 'database',
		description: 'monitorType.redis.desc',
		fields: [
			{ ...hostField, placeholder: 'cache.internal' },
			portField(6379),
			{ key: 'username', label: 'configField.username', type: 'text' },
			passwordField,
			{
				// An int db index here, unlike the string `database` of the SQL types.
				key: 'database',
				label: 'configField.redisDatabase',
				type: 'number',
				default: 0,
				min: 0
			},
			{ key: 'tls', label: 'configField.tls', type: 'boolean', default: false },
			{
				key: 'tls_skip_verify',
				label: 'configField.tlsSkipVerify',
				type: 'boolean',
				default: false,
				showIf: { key: 'tls', equals: true }
			},
			connectTimeoutField()
		]
	},
	{
		type: 'docker',
		label: 'monitorType.docker',
		family: 'service',
		description: 'monitorType.docker.desc',
		fields: [
			{
				key: 'endpoint',
				label: 'configField.dockerEndpoint',
				type: 'text',
				placeholder: 'unix:///var/run/docker.sock',
				hint: 'configField.dockerEndpoint.hint'
			},
			{ key: 'container', label: 'configField.container', type: 'text', required: true },
			{
				key: 'required_state',
				label: 'configField.requiredState',
				type: 'select',
				default: 'running',
				options: [
					{ value: 'running', label: 'running', labelKey: 'configOption.stateRunning' },
					{ value: 'healthy', label: 'healthy', labelKey: 'configOption.stateHealthy' }
				]
			},
			{ key: 'tls_ca', label: 'configField.tlsCa', type: 'textarea' },
			{ key: 'tls_cert', label: 'configField.clientCert', type: 'textarea' },
			{ key: 'tls_key', label: 'configField.clientKey', type: 'textarea' },
			ignoreTlsField,
			connectTimeoutField()
		]
	},
	{
		type: 'grpc',
		label: 'monitorType.grpc',
		family: 'service',
		description: 'monitorType.grpc.desc',
		fields: [
			{
				key: 'target',
				label: 'configField.grpcTarget',
				type: 'text',
				required: true,
				placeholder: 'api.internal:50051',
				hint: 'configField.grpcTarget.hint'
			},
			{
				key: 'service',
				label: 'configField.grpcService',
				type: 'text',
				hint: 'configField.grpcService.hint'
			},
			{
				key: 'expected_status',
				label: 'configField.expectedStatus',
				type: 'select',
				default: 'SERVING',
				options: tokens('SERVING', 'NOT_SERVING', 'UNKNOWN', 'SERVICE_UNKNOWN')
			},
			{ key: 'tls', label: 'configField.tls', type: 'boolean', default: false },
			{
				key: 'tls_ca',
				label: 'configField.tlsCa',
				type: 'textarea',
				showIf: { key: 'tls', equals: true }
			},
			{
				key: 'server_name',
				label: 'configField.serverName',
				type: 'text',
				showIf: { key: 'tls', equals: true }
			},
			{ ...ignoreTlsField, showIf: { key: 'tls', equals: true } },
			connectTimeoutField()
		]
	},
	{
		type: 'mqtt',
		label: 'monitorType.mqtt',
		family: 'service',
		description: 'monitorType.mqtt.desc',
		fields: [
			{
				key: 'broker',
				label: 'configField.broker',
				type: 'text',
				required: true,
				placeholder: 'tcp://broker.internal:1883',
				hint: 'configField.broker.hint'
			},
			{ key: 'client_id', label: 'configField.clientId', type: 'text' },
			{ key: 'username', label: 'configField.username', type: 'text' },
			passwordField,
			{ key: 'topic', label: 'configField.topic', type: 'text', placeholder: 'statwarden/probe' },
			{
				key: 'publish_payload',
				label: 'configField.publishPayload',
				type: 'text',
				hint: 'configField.needsTopic.hint'
			},
			{
				key: 'expected_payload',
				label: 'configField.expectedPayload',
				type: 'text',
				hint: 'configField.needsTopic.hint'
			},
			{
				key: 'tls_ca',
				label: 'configField.tlsCa',
				type: 'textarea',
				hint: 'configField.mqttTls.hint'
			},
			{ key: 'server_name', label: 'configField.serverName', type: 'text' },
			ignoreTlsField,
			connectTimeoutField()
		]
	},
	{
		type: 'steam',
		label: 'monitorType.steam',
		family: 'service',
		description: 'monitorType.steam.desc',
		fields: [
			hostField,
			// Required: steam is the one checker with no default port.
			{ ...portField(27015, true), placeholder: '27015' },
			connectTimeoutField(5)
		]
	},
	{
		type: 'push',
		label: 'monitorType.push',
		family: 'push',
		description: 'monitorType.push.desc',
		fields: [
			{
				key: 'expected_interval_seconds',
				label: 'configField.expectedInterval',
				type: 'number',
				default: 3600,
				required: true,
				min: 1
			},
			{
				key: 'grace_period_seconds',
				label: 'configField.gracePeriod',
				type: 'number',
				default: 300,
				min: 0,
				hint: 'configField.gracePeriod.hint'
			}
		]
	}
];

// ---- Notification providers ------------------------------------------------

/** Every chat provider spells this the same way — except `webhook`, which uses `url`. */
const webhookUrlField: FieldSpec = {
	key: 'webhook_url',
	label: 'configField.webhookUrl',
	type: 'url',
	required: true
};

export const notificationTypeSpecs: NotificationTypeSpec[] = [
	{
		// Registered as "smtp", not "email".
		type: 'smtp',
		label: 'notifyType.smtp',
		description: 'notifyType.smtp.desc',
		fields: [
			{ ...hostField, placeholder: 'smtp.example.com' },
			{ ...portField(587), hint: 'configField.smtpPort.hint' },
			{ key: 'username', label: 'configField.username', type: 'text' },
			passwordField,
			{
				key: 'from',
				label: 'configField.from',
				type: 'text',
				required: true,
				placeholder: 'statwarden@example.com'
			},
			{
				key: 'to',
				label: 'configField.recipients',
				type: 'textarea',
				shape: 'stringlist',
				required: true,
				hint: 'configField.list.hint'
			},
			{
				key: 'encryption',
				label: 'configField.encryption',
				type: 'select',
				default: 'starttls',
				options: tokens('none', 'starttls', 'tls')
			}
		]
	},
	{
		type: 'slack',
		label: 'notifyType.slack',
		description: 'notifyType.slack.desc',
		fields: [
			webhookUrlField,
			{ key: 'channel', label: 'configField.channel', type: 'text', placeholder: '#alerts' },
			{ key: 'username', label: 'configField.senderName', type: 'text' },
			{
				key: 'icon_emoji',
				label: 'configField.iconEmoji',
				type: 'text',
				placeholder: ':rotating_light:'
			},
			{
				key: 'icon_url',
				label: 'configField.iconUrl',
				type: 'url',
				hint: 'configField.slackIcon.hint'
			}
		]
	},
	{
		type: 'discord',
		label: 'notifyType.discord',
		description: 'notifyType.discord.desc',
		fields: [
			webhookUrlField,
			{ key: 'username', label: 'configField.senderName', type: 'text' },
			{ key: 'avatar_url', label: 'configField.avatarUrl', type: 'url' }
		]
	},
	{
		type: 'telegram',
		label: 'notifyType.telegram',
		description: 'notifyType.telegram.desc',
		fields: [
			{
				key: 'bot_token',
				label: 'configField.botToken',
				type: 'password',
				required: true,
				hint: 'configField.botToken.hint'
			},
			{
				// A string on the wire: a JSON number fails to unmarshal.
				key: 'chat_id',
				label: 'configField.chatId',
				type: 'text',
				required: true,
				placeholder: '-1001234567890'
			}
		]
	},
	{
		type: 'teams',
		label: 'notifyType.teams',
		description: 'notifyType.teams.desc',
		fields: [webhookUrlField]
	},
	{
		type: 'googlechat',
		label: 'notifyType.googlechat',
		description: 'notifyType.googlechat.desc',
		fields: [webhookUrlField]
	},
	{
		type: 'mattermost',
		label: 'notifyType.mattermost',
		description: 'notifyType.mattermost.desc',
		fields: [
			webhookUrlField,
			{ key: 'channel', label: 'configField.channel', type: 'text', placeholder: 'town-square' },
			{ key: 'username', label: 'configField.senderName', type: 'text' },
			{ key: 'icon_url', label: 'configField.iconUrl', type: 'url' },
			{
				key: 'icon_emoji',
				label: 'configField.iconEmoji',
				type: 'text',
				placeholder: ':rotating_light:'
			}
		]
	},
	{
		type: 'rocketchat',
		label: 'notifyType.rocketchat',
		description: 'notifyType.rocketchat.desc',
		fields: [
			webhookUrlField,
			{ key: 'channel', label: 'configField.channel', type: 'text', placeholder: '#alerts' },
			{ key: 'username', label: 'configField.senderName', type: 'text' },
			// Spelled `emoji` here, not `icon_emoji`.
			{
				key: 'emoji',
				label: 'configField.iconEmoji',
				type: 'text',
				placeholder: ':rotating_light:'
			}
		]
	},
	{
		type: 'pagerduty',
		label: 'notifyType.pagerduty',
		description: 'notifyType.pagerduty.desc',
		fields: [
			{ key: 'routing_key', label: 'configField.routingKey', type: 'password', required: true },
			{ key: 'source', label: 'configField.source', type: 'text', hint: 'configField.source.hint' }
		]
	},
	{
		type: 'opsgenie',
		label: 'notifyType.opsgenie',
		description: 'notifyType.opsgenie.desc',
		fields: [
			{ key: 'api_key', label: 'configField.apiKey', type: 'password', required: true },
			{
				key: 'region',
				label: 'configField.region',
				type: 'select',
				default: 'us',
				options: tokens('us', 'eu')
			}
		]
	},
	{
		type: 'ntfy',
		label: 'notifyType.ntfy',
		description: 'notifyType.ntfy.desc',
		fields: [
			{
				key: 'server_url',
				label: 'configField.serverUrl',
				type: 'url',
				default: 'https://ntfy.sh'
			},
			{ key: 'topic', label: 'configField.topic', type: 'text', required: true },
			{
				key: 'token',
				label: 'configField.accessToken',
				type: 'password',
				hint: 'configField.ntfyToken.hint'
			},
			{ key: 'username', label: 'configField.username', type: 'text' },
			passwordField,
			{
				key: 'priority',
				label: 'configField.priority',
				type: 'number',
				min: 0,
				max: 5,
				hint: 'configField.ntfyPriority.hint'
			},
			{
				key: 'tags',
				label: 'configField.tags',
				type: 'text',
				shape: 'stringlist',
				placeholder: 'warning, skull',
				hint: 'configField.list.hint'
			}
		]
	},
	{
		type: 'pushover',
		label: 'notifyType.pushover',
		description: 'notifyType.pushover.desc',
		fields: [
			{ key: 'token', label: 'configField.appToken', type: 'password', required: true },
			{ key: 'user', label: 'configField.userKey', type: 'text', required: true },
			{ key: 'device', label: 'configField.device', type: 'text' },
			{
				key: 'priority',
				label: 'configField.priority',
				type: 'number',
				min: -2,
				max: 2,
				hint: 'configField.pushoverPriority.hint'
			}
		]
	},
	{
		type: 'gotify',
		label: 'notifyType.gotify',
		description: 'notifyType.gotify.desc',
		fields: [
			{
				key: 'server_url',
				label: 'configField.serverUrl',
				type: 'url',
				required: true,
				placeholder: 'https://gotify.example.com'
			},
			{ key: 'app_token', label: 'configField.appToken', type: 'password', required: true },
			{
				key: 'priority',
				label: 'configField.priority',
				type: 'number',
				min: 0,
				max: 10,
				hint: 'configField.gotifyPriority.hint'
			}
		]
	},
	{
		type: 'matrix',
		label: 'notifyType.matrix',
		description: 'notifyType.matrix.desc',
		fields: [
			{
				key: 'homeserver_url',
				label: 'configField.homeserverUrl',
				type: 'url',
				required: true,
				placeholder: 'https://matrix.example.org'
			},
			{ key: 'access_token', label: 'configField.accessToken', type: 'password', required: true },
			{
				key: 'room_id',
				label: 'configField.roomId',
				type: 'text',
				required: true,
				placeholder: '!abcdef:example.org'
			}
		]
	},
	{
		type: 'signal',
		label: 'notifyType.signal',
		description: 'notifyType.signal.desc',
		fields: [
			{
				key: 'server_url',
				label: 'configField.serverUrl',
				type: 'url',
				required: true,
				placeholder: 'http://signal-cli:8080',
				hint: 'configField.signalServer.hint'
			},
			{
				key: 'number',
				label: 'configField.senderNumber',
				type: 'text',
				required: true,
				placeholder: '+4915112345678'
			},
			{
				key: 'recipients',
				label: 'configField.recipients',
				type: 'textarea',
				shape: 'stringlist',
				required: true,
				hint: 'configField.list.hint'
			}
		]
	},
	{
		type: 'twilio',
		label: 'notifyType.twilio',
		description: 'notifyType.twilio.desc',
		fields: [
			{ key: 'account_sid', label: 'configField.accountSid', type: 'text', required: true },
			{ key: 'auth_token', label: 'configField.authToken', type: 'password', required: true },
			{
				key: 'from',
				label: 'configField.senderNumber',
				type: 'text',
				required: true,
				placeholder: '+15551234567'
			},
			{
				key: 'to',
				label: 'configField.recipients',
				type: 'textarea',
				shape: 'stringlist',
				required: true,
				hint: 'configField.list.hint'
			}
		]
	},
	{
		type: 'apprise',
		label: 'notifyType.apprise',
		description: 'notifyType.apprise.desc',
		fields: [
			{
				key: 'server_url',
				label: 'configField.serverUrl',
				type: 'url',
				required: true,
				placeholder: 'http://apprise:8000'
			},
			{
				key: 'urls',
				label: 'configField.appriseUrls',
				type: 'textarea',
				shape: 'stringlist',
				hint: 'configField.appriseEither.hint'
			},
			{
				key: 'config_key',
				label: 'configField.configKey',
				type: 'text',
				hint: 'configField.appriseEither.hint'
			},
			{ key: 'tag', label: 'configField.tag', type: 'text', hint: 'configField.appriseTag.hint' }
		]
	},
	{
		type: 'webpush',
		label: 'notifyType.webpush',
		description: 'notifyType.webpush.desc',
		fields: [
			{ key: 'vapid_public_key', label: 'configField.vapidPublic', type: 'text', required: true },
			{
				key: 'vapid_private_key',
				label: 'configField.vapidPrivate',
				type: 'password',
				required: true
			},
			{
				key: 'subject',
				label: 'configField.webpushSubject',
				type: 'text',
				required: true,
				placeholder: 'mailto:ops@example.com',
				hint: 'configField.webpushSubject.hint'
			},
			{
				// A nested object, not a string: the browser's PushSubscription.
				key: 'subscription',
				label: 'configField.subscription',
				type: 'textarea',
				shape: 'json',
				required: true,
				hint: 'configField.subscription.hint'
			}
		]
	},
	{
		type: 'webhook',
		label: 'notifyType.webhook',
		description: 'notifyType.webhook.desc',
		fields: [
			// `url`, not `webhook_url` — the one provider that differs.
			{ key: 'url', label: 'configField.url', type: 'url', required: true },
			{
				key: 'method',
				label: 'configField.method',
				type: 'select',
				default: 'POST',
				options: tokens('POST', 'PUT', 'PATCH', 'GET')
			},
			{
				key: 'headers',
				label: 'configField.headers',
				type: 'textarea',
				shape: 'keyvalue',
				placeholder: 'Authorization: Bearer token',
				hint: 'configField.headers.hint'
			}
		]
	}
];

// ---- Form <-> wire conversion ----------------------------------------------

/**
 * Whether a field's `showIf` is satisfied by the current form state. Shared so
 * that "is this field rendered" and "is this field required" can never disagree
 * — a hidden required field that still blocks the save is unfixable from the UI.
 */
export function isFieldVisible(f: FieldSpec, config: Record<string, unknown>): boolean {
	if (!f.showIf) return true;
	const actual = config[f.showIf.key];
	if (f.showIf.in) return f.showIf.in.includes(actual as string | number | boolean);
	return actual === f.showIf.equals;
}

/** Entries are separated by newlines or commas; blanks are dropped. */
function parseList(raw: string): string[] {
	return raw
		.split(/[\n,]/)
		.map((s) => s.trim())
		.filter(Boolean);
}

/**
 * `200-299, 301` -> `["200-299", 301]`. Single codes go out as numbers and
 * ranges as "lo-hi" strings, which is exactly what StatusCodeSet.UnmarshalJSON
 * accepts and what it marshals back. Tokens that are neither are passed through
 * untouched so the server's error names the value the user actually typed.
 */
function parseStatusCodes(raw: string): (number | string)[] {
	return parseList(raw).map((token) => {
		if (/^\d+$/.test(token)) return Number(token);
		const range = token.match(/^(\d+)\s*-\s*(\d+)$/);
		return range ? `${range[1]}-${range[2]}` : token;
	});
}

/** One `Name: value` per line. The first colon splits; later ones stay in the value. */
function parseKeyValue(raw: string): Record<string, string> {
	const out: Record<string, string> = {};
	for (const line of raw.split('\n')) {
		const trimmed = line.trim();
		if (!trimmed) continue;
		const colon = trimmed.indexOf(':');
		// A nameless header is rejected by Validate; keep the whole line as the
		// name so the resulting error points at the offending line.
		if (colon < 0) {
			out[trimmed] = '';
			continue;
		}
		out[trimmed.slice(0, colon).trim()] = trimmed.slice(colon + 1).trim();
	}
	return out;
}

function formatValue(shape: FieldSpec['shape'], value: unknown): string {
	if (value == null) return '';
	switch (shape) {
		case 'stringlist':
			return Array.isArray(value) ? value.join('\n') : String(value);
		case 'statuscodes':
			return Array.isArray(value) ? value.join(', ') : String(value);
		case 'keyvalue':
			return typeof value === 'object'
				? Object.entries(value as Record<string, string>)
						.map(([k, v]) => `${k}: ${v}`)
						.join('\n')
				: String(value);
		case 'json':
			return typeof value === 'object' ? JSON.stringify(value, null, 2) : String(value);
		default:
			return String(value);
	}
}

/**
 * Wire config -> form state. Arrays and objects become the text their widget
 * can hold; everything else is passed through. Keys the spec does not know
 * about are kept so an unrecognised field is not silently destroyed by an edit.
 */
export function configToForm(
	fields: FieldSpec[],
	config: Record<string, unknown>
): Record<string, unknown> {
	const out: Record<string, unknown> = { ...config };
	for (const f of fields) {
		if (!f.shape) continue;
		if (config[f.key] === undefined) continue;
		out[f.key] = formatValue(f.shape, config[f.key]);
	}
	return out;
}

/**
 * Form state -> wire config, the only place a config is made ready to POST.
 *
 * Besides applying `shape`, this drops three classes of value that the Go
 * decoder rejects or misreads: keys that are not in the current type's spec
 * (left behind when the user switches type mid-form, e.g. an `oid` on an http
 * monitor); fields hidden by an unsatisfied `showIf` (a `basic_password` left
 * behind after switching auth back to none — `showIf` only hides the widget, so
 * without this the stale secret still ships and the server's Validate() rejects
 * the now-inconsistent config); and empty strings, which would arrive as
 * `"port": ""` and fail to decode into an int. Booleans are always kept —
 * `follow_redirects: false` is meaningfully different from an absent one.
 */
export function configFromForm(
	fields: FieldSpec[],
	form: Record<string, unknown>
): Record<string, unknown> {
	const out: Record<string, unknown> = {};
	for (const f of fields) {
		// A field the user cannot see must not be sent: its value is stale the
		// moment the controlling field moves away from the value that revealed it.
		if (!isFieldVisible(f, form)) continue;
		const raw = form[f.key];
		if (raw === undefined || raw === null) continue;
		if (typeof raw === 'boolean') {
			out[f.key] = raw;
			continue;
		}
		const text = String(raw).trim();
		if (text === '') continue;

		switch (f.shape) {
			case 'stringlist': {
				const list = parseList(text);
				if (list.length) out[f.key] = list;
				break;
			}
			case 'statuscodes': {
				const codes = parseStatusCodes(text);
				if (codes.length) out[f.key] = codes;
				break;
			}
			case 'keyvalue': {
				const map = parseKeyValue(text);
				if (Object.keys(map).length) out[f.key] = map;
				break;
			}
			case 'json': {
				// Invalid JSON is forwarded as the raw string: the server answers
				// with a decode error naming the field, which beats dropping the
				// value and reporting a missing required field instead.
				try {
					out[f.key] = JSON.parse(text);
				} catch {
					out[f.key] = text;
				}
				break;
			}
			default:
				out[f.key] = f.type === 'number' ? Number(text) : raw;
		}
	}
	return out;
}
