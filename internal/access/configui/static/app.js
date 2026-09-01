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
    byId('app-id').value = settings.app_id || '';
    byId('executor-id').value = settings.executor_id || '';
    byId('executor-config').value = JSON.stringify(settings.executor_config || {}, null, 2);
    byId('executor-timeout').value = settings.executor_timeout_seconds;
    byId('qq-enabled').checked = settings.qq_enabled;
    byId('qq-ws-url').value = settings.qq_ws_url || '';
    byId('qq-bot-id').value = settings.qq_bot_id || '';
    byId('qq-groups').value = (settings.qq_allowed_group_ids || []).join('\n');
    byId('qq-private-users').value = (settings.qq_allowed_private_user_ids || []).join('\n');
    byId('qq-quick-replies').value = quickReplyLines(settings.qq_quick_replies);
    byId('qq-poke-replies').value = (settings.qq_poke_replies || []).join('\n');
    byId('execution-max-steps').value = settings.execution?.max_steps;
    byId('execution-max-capability-calls').value = settings.execution?.max_capability_calls;
    byId('execution-max-units').value = settings.execution?.max_execution_units;
    byId('execution-max-output-bytes').value = settings.execution?.max_output_bytes;
    byId('run-timeout').value = settings.orchestration?.run_timeout_seconds;
    byId('run-max-attempts').value = settings.orchestration?.max_run_attempts;
    byId('run-queue-capacity').value = settings.orchestration?.queue_capacity;
    byId('run-max-call-depth').value = settings.orchestration?.max_call_depth;
    byId('context-max-messages').value = settings.context_assembly?.max_messages;
    byId('context-max-chars-per-msg').value = settings.context_assembly?.max_chars_per_msg;
    byId('context-max-total-chars').value = settings.context_assembly?.max_total_chars;
    byId('context-max-bytes').value = settings.context_assembly?.max_context_bytes;
    byId('scheduler-workers').value = settings.scheduler?.workers;
    byId('scheduler-poll-ms').value = settings.scheduler?.poll_ms;
    byId('scheduler-batch-size').value = settings.scheduler?.batch_size;
    byId('governance-confirmation-sweep').value = settings.governance?.confirmation_sweep_seconds;
    byId('qq-dial-timeout').value = settings.qq_connection?.dial_timeout_seconds;
    byId('qq-reconnect-delay').value = settings.qq_connection?.reconnect_delay_seconds;
    byId('qq-run-timeout').value = settings.qq_connection?.run_timeout_seconds;
    byId('qq-manager-stop-timeout').value = settings.qq_connection?.manager_stop_timeout_seconds;
    byId('runtime-dial-timeout').value = settings.runtime_process?.dial_timeout_seconds;
    byId('runtime-stop-grace').value = settings.runtime_process?.stop_grace_seconds;
    byId('runtime-terminate-grace').value = settings.runtime_process?.terminate_grace_seconds;
  }
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
  let executorConfig;
  try {
    quickReplies = parseQuickReplies(byId('qq-quick-replies').value);
    executorConfig = JSON.parse(byId('executor-config').value);
    if (!executorConfig || typeof executorConfig !== 'object' || Array.isArray(executorConfig)) {
      throw new Error('执行者配置必须是 JSON 对象');
    }
  } catch (error) {
    setNotice(error.message, 'error');
    saveButton.disabled = false;
    return;
  }
  const payload = {
    revision,
    app_id: byId('app-id').value.trim(),
    executor_id: byId('executor-id').value.trim(),
    executor_config: executorConfig,
    executor_timeout_seconds: Number(byId('executor-timeout').value),
    qq_enabled: byId('qq-enabled').checked,
    qq_ws_url: byId('qq-ws-url').value.trim(),
    qq_ws_token: byId('qq-token').value,
    clear_qq_ws_token: byId('clear-qq-token').checked,
    qq_bot_id: byId('qq-bot-id').value.trim(),
    qq_allowed_group_ids: listValue(byId('qq-groups').value),
    qq_allowed_private_user_ids: listValue(byId('qq-private-users').value),
    qq_quick_replies: quickReplies,
    qq_poke_replies: listValue(byId('qq-poke-replies').value),
    execution: {
      max_steps: Number(byId('execution-max-steps').value),
      max_capability_calls: Number(byId('execution-max-capability-calls').value),
      max_execution_units: Number(byId('execution-max-units').value),
      max_output_bytes: Number(byId('execution-max-output-bytes').value)
    },
    orchestration: {
      run_timeout_seconds: Number(byId('run-timeout').value),
      max_run_attempts: Number(byId('run-max-attempts').value),
      queue_capacity: Number(byId('run-queue-capacity').value),
      max_call_depth: Number(byId('run-max-call-depth').value)
    },
    context_assembly: {
      max_messages: Number(byId('context-max-messages').value),
      max_chars_per_msg: Number(byId('context-max-chars-per-msg').value),
      max_total_chars: Number(byId('context-max-total-chars').value),
      max_context_bytes: Number(byId('context-max-bytes').value)
    },
    scheduler: {
      workers: Number(byId('scheduler-workers').value),
      poll_ms: Number(byId('scheduler-poll-ms').value),
      batch_size: Number(byId('scheduler-batch-size').value)
    },
    qq_connection: {
      dial_timeout_seconds: Number(byId('qq-dial-timeout').value),
      reconnect_delay_seconds: Number(byId('qq-reconnect-delay').value),
      run_timeout_seconds: Number(byId('qq-run-timeout').value),
      manager_stop_timeout_seconds: Number(byId('qq-manager-stop-timeout').value)
    },
    runtime_process: {
      dial_timeout_seconds: Number(byId('runtime-dial-timeout').value),
      stop_grace_seconds: Number(byId('runtime-stop-grace').value),
      terminate_grace_seconds: Number(byId('runtime-terminate-grace').value)
    },
    governance: {
      confirmation_sweep_seconds: Number(byId('governance-confirmation-sweep').value)
    }
  };
  try {
    const snapshot = await readJSON(await fetch('/api/v1/config', {method: 'PUT', headers: {'Content-Type': 'application/json', Accept: 'application/json'}, body: JSON.stringify(payload)}));
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
