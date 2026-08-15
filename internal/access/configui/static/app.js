const byId = id => document.getElementById(id);
const form = byId('settings-form');
const saveButton = byId('save-button');
const notice = byId('notice');
let revision = 0;

function listValue(value) {
  return value.split(/[\n,]/).map(item => item.trim()).filter(Boolean);
}

function quickReplyLines(rules) {
  return (rules || []).map(rule => `${rule.trigger} => ${rule.reply}`).join('\n');
}

function parseQuickReplies(value) {
  return value.split('\n').map(line => line.trim()).filter(Boolean).map(line => {
    const separator = line.indexOf('=>');
    if (separator < 1) throw new Error(`快速回复格式错误：${line}`);
    const trigger = line.slice(0, separator).trim();
    const reply = line.slice(separator + 2).trim();
    if (!trigger || !reply) throw new Error(`快速回复格式错误：${line}`);
    return {trigger, reply};
  });
}

function setNotice(message, kind = '') {
  notice.textContent = message;
  notice.className = kind;
}

function render(snapshot, preserveInputs = false) {
  const settings = snapshot.settings;
  revision = settings.revision;
  if (!preserveInputs) {
    byId('model').value = settings.model || '';
    byId('model-base-url').value = settings.model_base_url || '';
    byId('request-timeout').value = settings.model_request_timeout_seconds;
    byId('readiness-timeout').value = settings.model_readiness_timeout_seconds;
    byId('max-retries').value = settings.model_max_retries;
    byId('retry-base').value = settings.model_retry_base_seconds;
    byId('retry-max').value = settings.model_retry_max_seconds;
    byId('requests-per-minute').value = settings.model_requests_per_minute;
    byId('max-concurrency').value = settings.model_max_concurrency;
    byId('qq-enabled').checked = settings.qq_enabled;
    byId('qq-ws-url').value = settings.qq_ws_url || '';
    byId('qq-bot-id').value = settings.qq_bot_id || '';
    byId('qq-groups').value = (settings.qq_allowed_group_ids || []).join('\n');
    byId('qq-private-users').value = (settings.qq_allowed_private_user_ids || []).join('\n');
    byId('qq-quick-replies').value = quickReplyLines(settings.qq_quick_replies);
    byId('qq-poke-replies').value = (settings.qq_poke_replies || []).join('\n');
  }
  byId('model-key-state').textContent = snapshot.model_api_key_configured ? '已安全保存' : '未配置';
  byId('qq-token-state').textContent = snapshot.qq_ws_token_configured ? '已安全保存' : '未配置';
  byId('revision').textContent = revision ? `配置修订 ${revision}` : '首次配置';
  byId('runtime-state').textContent = ({ready: '运行中', starting: '启动中', restarting: '应用中', setup_required: '等待配置', failed: '启动失败'})[snapshot.runtime.state] || snapshot.runtime.state;
  byId('runtime-message').textContent = snapshot.runtime.message;
  document.body.dataset.runtime = snapshot.runtime.state;
}

async function readJSON(response) {
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.message || '请求失败');
  return data;
}

async function loadSettings(preserveInputs = false) {
  try {
    render(await readJSON(await fetch('/api/v1/config', {headers: {Accept: 'application/json'}})), preserveInputs);
  } catch (error) {
    setNotice(`读取配置失败：${error.message}`, 'error');
  }
}

form.addEventListener('submit', async event => {
  event.preventDefault();
  saveButton.disabled = true;
  setNotice('正在安全保存，并通知主进程应用配置…');
  let quickReplies;
  try {
    quickReplies = parseQuickReplies(byId('qq-quick-replies').value);
  } catch (error) {
    setNotice(error.message, 'error');
    saveButton.disabled = false;
    return;
  }
  const payload = {
    revision,
    model: byId('model').value.trim(),
    model_base_url: byId('model-base-url').value.trim(),
    model_api_key: byId('model-api-key').value,
    model_request_timeout_seconds: Number(byId('request-timeout').value),
    model_readiness_timeout_seconds: Number(byId('readiness-timeout').value),
    model_max_retries: Number(byId('max-retries').value),
    model_retry_base_seconds: Number(byId('retry-base').value),
    model_retry_max_seconds: Number(byId('retry-max').value),
    model_requests_per_minute: Number(byId('requests-per-minute').value),
    model_max_concurrency: Number(byId('max-concurrency').value),
    qq_enabled: byId('qq-enabled').checked,
    qq_ws_url: byId('qq-ws-url').value.trim(),
    qq_ws_token: byId('qq-token').value,
    clear_qq_ws_token: byId('clear-qq-token').checked,
    qq_bot_id: byId('qq-bot-id').value.trim(),
    qq_allowed_group_ids: listValue(byId('qq-groups').value),
    qq_allowed_private_user_ids: listValue(byId('qq-private-users').value),
    qq_quick_replies: quickReplies,
    qq_poke_replies: listValue(byId('qq-poke-replies').value)
  };
  try {
    const snapshot = await readJSON(await fetch('/api/v1/config', {method: 'PUT', headers: {'Content-Type': 'application/json', Accept: 'application/json'}, body: JSON.stringify(payload)}));
    byId('model-api-key').value = '';
    byId('qq-token').value = '';
    byId('clear-qq-token').checked = false;
    render(snapshot);
    setNotice('配置已保存，AI珞正在应用新的运行参数。', 'success');
  } catch (error) {
    setNotice(error.message, 'error');
    await loadSettings(true);
  } finally {
    saveButton.disabled = false;
  }
});

loadSettings();
setInterval(() => loadSettings(true), 3000);
