const byId = id => document.getElementById(id);
const form = byId('settings-form');
const saveButton = byId('save-button');
const notice = byId('notice');
const promptCatalogEditor = byId('prompt-catalog-editor');
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

function escapeHTML(value) {
  return String(value ?? '').replace(/[&<>"']/g, character => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  })[character]);
}

function renderPromptCatalog(catalog, preserveInputs = false) {
  if (preserveInputs) return;
  const styles = (catalog?.basic_styles || []).map(style => `
    <div class="prompt-card" data-style-key="${escapeHTML(style.key)}">
      <div class="prompt-card-head">
        <strong>基本风格 · ${escapeHTML(style.key)}</strong>
        <input data-field="style-name" value="${escapeHTML(style.name)}" maxlength="64" aria-label="${escapeHTML(style.key)} 名称">
      </div>
      <textarea data-field="style-text" rows="5" maxlength="2048" aria-label="${escapeHTML(style.key)} 提示词正文">${escapeHTML(style.text)}</textarea>
    </div>`).join('');
  const traits = (catalog?.extra_traits || []).map(trait => `
    <div class="prompt-card prompt-trait-card" data-trait-key="${escapeHTML(trait.key)}">
      <div class="prompt-card-head">
        <strong>额外特征 · ${escapeHTML(trait.key)}</strong>
        <input data-field="trait-name" value="${escapeHTML(trait.name)}" maxlength="64" aria-label="${escapeHTML(trait.key)} 名称">
      </div>
      <div class="trait-level-grid">
        <label><span>增强</span><textarea data-level="enhanced" rows="5" maxlength="2048">${escapeHTML(trait.enhanced)}</textarea></label>
        <label><span>默认</span><textarea data-level="default" rows="5" maxlength="2048">${escapeHTML(trait.default)}</textarea></label>
        <label><span>减弱</span><textarea data-level="reduced" rows="5" maxlength="2048">${escapeHTML(trait.reduced)}</textarea></label>
      </div>
    </div>`).join('');
  promptCatalogEditor.innerHTML = `<div class="prompt-catalog-block">${styles}</div><div class="prompt-catalog-block">${traits}</div>`;
}

function collectPromptCatalog() {
  const basicStyles = [...promptCatalogEditor.querySelectorAll('[data-style-key]')].map(card => {
    const key = card.dataset.styleKey;
    const name = card.querySelector('[data-field="style-name"]').value.trim();
    const text = card.querySelector('[data-field="style-text"]').value.trim();
    if (!name || !text) throw new Error(`基本风格 ${key} 的名称和正文不能为空`);
    return {key, name, text};
  });
  const extraTraits = [...promptCatalogEditor.querySelectorAll('[data-trait-key]')].map(card => {
    const key = card.dataset.traitKey;
    const name = card.querySelector('[data-field="trait-name"]').value.trim();
    const levels = {};
    for (const field of card.querySelectorAll('[data-level]')) {
      const value = field.value.trim();
      if (!value) throw new Error(`额外特征 ${key} 的「${field.dataset.level}」档不能为空`);
      levels[field.dataset.level] = value;
    }
    if (!name) throw new Error(`额外特征 ${key} 的名称不能为空`);
    return {key, name, ...levels};
  });
  return {basic_styles: basicStyles, extra_traits: extraTraits};
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
    byId('prompt-base').value = settings.base_system_prompt || '';
  }
  renderPromptCatalog(settings.prompt_catalog, preserveInputs);
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
  let promptCatalog;
  try {
    quickReplies = parseQuickReplies(byId('qq-quick-replies').value);
    promptCatalog = collectPromptCatalog();
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
    qq_poke_replies: listValue(byId('qq-poke-replies').value),
    prompt_catalog: promptCatalog,
    base_system_prompt: byId('prompt-base').value.trim()
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
