const statusNode = document.querySelector('#status');
const output = document.querySelector('#output');
const cancelButton = document.querySelector('#cancel');
let currentEcho = null;
let events = null;

fetch('/readyz').then(response => response.json()).then(data => {
  statusNode.textContent = `服务状态：${data.status}`;
}).catch(error => { statusNode.textContent = `服务不可用：${error}`; });

document.querySelector('#form').addEventListener('submit', async event => {
  event.preventDefault();
  output.textContent = '';
  const response = await fetch('/api/v2/echoes', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Idempotency-Key': crypto.randomUUID(),
    },
    body: JSON.stringify({message: document.querySelector('#message').value}),
  });
  const body = await response.json();
  if (!response.ok) {
    output.textContent = JSON.stringify(body, null, 2);
    return;
  }
  currentEcho = body.echo_id;
  cancelButton.disabled = false;
  events?.close();
  events = new EventSource(body.events_url);
  events.addEventListener('reply.delta', event => {
    output.textContent += JSON.parse(event.data).payload.text;
  });
  events.addEventListener('reply.final', () => {
    cancelButton.disabled = true;
    events.close();
  });
  events.addEventListener('run.failed', event => {
    output.textContent += `\n${JSON.parse(event.data).payload.message}`;
    cancelButton.disabled = true;
    events.close();
  });
});

cancelButton.addEventListener('click', async () => {
  if (!currentEcho) return;
  await fetch(`/api/v1/echoes/${currentEcho}`, {method: 'DELETE'});
  cancelButton.disabled = true;
});
