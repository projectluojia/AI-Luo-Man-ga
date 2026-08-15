const form = document.querySelector('#qq-form');
const enabled = document.querySelector('#enabled');
const wsURL = document.querySelector('#ws-url');
const botQQID = document.querySelector('#bot-qq-id');
const groups = document.querySelector('#groups');
const privateUsers = document.querySelector('#private-users');
const saveButton = document.querySelector('#save');
const notice = document.querySelector('#notice');
const connection = document.querySelector('#connection');
const statusDetail = document.querySelector('#status-detail');
const revision = document.querySelector('#revision');
let generation = 0;

function listValue(value) {
  return value.split(/[\n,]/).map(item => item.trim()).filter(Boolean);
}

function showNotice(message, kind = '') {
  notice.textContent = message;
  notice.className = `notice ${kind}`;
}

function render(data, preserveForm = false) {
  const settings = data.settings;
  const runtime = data.runtime;
  generation = settings.generation;
  if (!preserveForm) {
    enabled.checked = settings.enabled;
    wsURL.value = settings.ws_url || '';
    botQQID.value = settings.bot_qq_id || '';
    groups.value = (settings.allowed_group_ids || []).join('\n');
    privateUsers.value = (settings.allowed_private_user_ids || []).join('\n');
  }
  connection.className = `connection ${runtime.connected ? 'online' : runtime.running ? 'connecting' : 'offline'}`;
  connection.querySelector('strong').textContent = runtime.connected ? '已连接' : runtime.running ? '连接中' : settings.enabled ? '未连接' : '未启用';
  statusDetail.textContent = runtime.connected ? '消息入口正在接收允许的 QQ 消息。' : settings.enabled ? '正在等待 NapCat 的 OneBot WebSocket。' : '启用后，AI珞才会连接 NapCat。';
  revision.textContent = `配置版本 ${generation}`;
}

async function loadSettings() {
  try {
    const response = await fetch('/api/v1/admin/qq-access', {headers: {'Accept': 'application/json'}});
    const data = await response.json();
    if (!response.ok) throw new Error(data.message || '读取配置失败');
    render(data);
  } catch (error) {
    connection.className = 'connection offline';
    connection.querySelector('strong').textContent = '不可用';
    statusDetail.textContent = error.message;
    showNotice('无法读取本机 QQ 接入配置。', 'error');
  }
}

form.addEventListener('submit', async event => {
  event.preventDefault();
  saveButton.disabled = true;
  showNotice('正在保存并重启 QQ 连接…');
  try {
    const response = await fetch('/api/v1/admin/qq-access', {
      method: 'PUT',
      headers: {'Content-Type': 'application/json', 'Accept': 'application/json'},
      body: JSON.stringify({generation, enabled: enabled.checked, ws_url: wsURL.value.trim(), bot_qq_id: botQQID.value.trim(), allowed_group_ids: listValue(groups.value), allowed_private_user_ids: listValue(privateUsers.value)})
    });
    const data = await response.json();
    if (!response.ok) throw new Error(data.message || '保存失败');
    render(data);
    showNotice('已保存，新的 QQ 接入配置已经生效。', 'success');
  } catch (error) {
    showNotice(error.message, 'error');
    await loadSettings();
  } finally {
    saveButton.disabled = false;
  }
});

loadSettings();
setInterval(loadSettings, 5000);
