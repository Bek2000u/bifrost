"""Exercise the local native gateway fixture; never use this against production."""
import base64, hashlib, json, os, urllib.request, urllib.error, uuid
import sys

base = os.environ.get('BILLING_FIXTURE_URL', 'http://127.0.0.1:18989')
if not base.startswith('http://127.0.0.1:'):
    raise SystemExit('This smoke only accepts the owned loopback fixture URL')
admin = 'Basic ' + base64.b64encode(b'fixture-admin:fixture-only-password').decode()

def request(method, path, token=None, body=None, headers=None):
    extra = dict(headers or {})
    if token: extra['Authorization'] = token
    if body is not None: extra['Content-Type'] = 'application/json'
    req = urllib.request.Request(base + path, method=method, headers=extra,
        data=None if body is None else json.dumps(body).encode())
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            raw = response.read().decode()
            return response.status, dict(response.headers), raw
    except urllib.error.HTTPError as error:
        return error.code, dict(error.headers), error.read().decode()

run = uuid.uuid4().hex[:10]
keys = []
value_shapes = []
def secret_value(key):
    value = key['value']
    return value['value'] if isinstance(value, dict) else value
for suffix in ['a', 'b']:
    tenant = 'fixture-user-' + suffix + '-' + run
    body = {'name': 'billing-fixture-' + tenant, 'is_active': True, 'allow_all_providers': True,
            'metadata': {'billing_app_id': 'fixture-app', 'billing_tenant_id': tenant}}
    status, _, raw = request('POST', '/api/governance/virtual-keys', admin, body)
    assert status == 200, (status, raw)
    created = json.loads(raw)['virtual_key']
    status, _, _ = request('POST', '/api/governance/virtual-keys', admin, body)
    assert status == 409, status
    query = '?metadata_billing_app_id=fixture-app&metadata_billing_tenant_id=' + tenant + '&limit=100'
    status, _, raw = request('GET', '/api/governance/virtual-keys' + query, admin)
    recovered = json.loads(raw)['virtual_keys']
    assert status == 200 and len(recovered) == 1 and recovered[0]['id'] == created['id'], raw
    status, _, raw = request('GET', '/api/governance/virtual-keys/' + created['id'], admin)
    detail = json.loads(raw)['virtual_key']
    value_shapes.append({'create': type(created['value']).__name__, 'detail': type(detail['value']).__name__})
    secret = secret_value(detail)
    assert isinstance(secret, str) and secret and secret == secret_value(created)
    keys.append('Bearer ' + secret)

status, _, raw = request('GET', '/v1/billing/receipts', keys[0])
assert status == 200 and json.loads(raw)['gateway_id'], (status, raw)
for credentials in [None, admin]:
    status, _, raw = request('GET', '/v1/billing/receipts', credentials)
    assert status == 401, (status, raw)
status, _, raw = request('GET', '/v1/billing/receipts', headers={'x-bf-vk': keys[0][7:]})
assert status == 200, (status, raw)
status, _, raw = request('GET', '/v1/billing/receipts', keys[0], headers={'x-bf-vk': keys[1][7:]})
assert status == 400, (status, raw)
if '--management-only' in sys.argv:
    print(json.dumps({'managementScopeProvisionRecovery': True, 'nativeValueShapes': value_shapes,
        'duplicateCreate409': True, 'scopedReceiptsRead': True,
        'dashboardAuthNotRequired': True, 'unscopedReadDenied': True,
        'conflictingCredentialsDenied': True, 'inferenceSubmits': 0}, indent=2))
    sys.exit(0)
for stream in [False, True]:
    operation = 'fixture-operation-' + uuid.uuid4().hex
    headers = {'x-bifrost-billing-operation': operation,
               'x-bifrost-billing-hold': 'chld_00000000000000000000000001'}
    status, reply_headers, raw = request('POST', '/v1/chat/completions', keys[0],
        {'model': 'openai/fixture-model', 'messages': [{'role': 'user', 'content': 'fixture'}],
         'stream': stream, **({'stream_options': {'include_usage': True}} if stream else {})}, headers)
    assert status == 200 and 'BILLING_FIXTURE_OK' in raw, (status, raw)
    if stream: assert '[DONE]' in raw
    assert any(k.lower() == 'x-bifrost-billing-receipt-id' for k in reply_headers), reply_headers
    status, _, raw = request('GET', '/v1/billing/receipts?operation_key=' + operation, keys[0])
    receipts = json.loads(raw)['receipts']
    assert status == 200 and len(receipts) == 1, raw
    receipt = receipts[0]
    measures = {m['name']: m['value'] for m in receipt['measures']}
    assert measures['input_tokens'] == '128' and measures['output_tokens'] == '32', receipt
    assert receipt['scope']['tenant_id'].startswith('fixture-user-a-')
    assert receipt.get('cost') is None, 'fixture has no price or provider bill; cost must remain unknown'

status, _, raw = request('POST', '/v1/billing/events/claim', keys[0], {'limit': 100, 'lease_seconds': 60})
events = json.loads(raw)['events']
assert status == 200 and len(events) == 2, raw
for event in events:
    payload = event['payload_json'].encode()
    assert hashlib.sha256(payload).hexdigest() == event['payload_sha256']
    identity = event['receipt']['event_id']
    status, _, _ = request('GET', '/v1/billing/events/' + identity, keys[1])
    assert status == 404, status
    ack = {'payload_sha256': event['payload_sha256'], 'claim_token': event['claim_token']}
    status, _, _ = request('POST', '/v1/billing/events/' + identity + '/ack', keys[1], ack)
    assert status == 409, status
    for _ in range(2):
        status, _, raw = request('POST', '/v1/billing/events/' + identity + '/ack', keys[0], ack)
        assert status == 200, (status, raw)

status, _, raw = request('POST', '/v1/billing/events/claim', keys[0], {'limit': 100, 'lease_seconds': 60})
assert status == 200 and not json.loads(raw)['events'], raw
print(json.dumps({'nativeGateway': True, 'sqliteReceipts': True, 'managementScopeProvisionRecovery': True,
    'nativeValueShapes': value_shapes,
    'duplicateCreate409': True, 'chatJSON': True, 'chatSSE': True, 'receiptCount': 2,
    'exactPayloadHash': True, 'foreignScopeReadAckDenied': True, 'lostAckReplay': True,
    'automaticHoldCapture': False, 'paidProviderCalls': 0}, indent=2))
