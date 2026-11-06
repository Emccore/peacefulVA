'use strict';

/* ================= 工具 ================= */
const $ = s => document.querySelector(s);
const enc = new TextEncoder();
const dec = new TextDecoder();

function b64e(buf) {
  const u = new Uint8Array(buf);
  let s = '';
  for (let i = 0; i < u.length; i += 0x8000) {
    s += String.fromCharCode(...u.subarray(i, i + 0x8000));
  }
  return btoa(s);
}
const b64d = s => Uint8Array.from(atob(s), ch => ch.charCodeAt(0));

function toast(msg, ms = 3000) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.add('show');
  clearTimeout(t._h);
  t._h = setTimeout(() => t.classList.remove('show'), ms);
}

function mediaTimeout(promise, ms) {
  return Promise.race([
    promise,
    new Promise((_, reject) => setTimeout(() => reject(new Error('timeout')), ms)),
  ]);
}

function nowHM() {
  const t = new Date();
  return `${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')}`;
}

async function api(path, body, method) {
  try {
    const res = await fetch(path, {
      method: method || (body ? 'POST' : 'GET'),
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
    const data = await res.json().catch(() => ({}));
    return { ok: res.ok, data };
  } catch (_) {
    return { ok: false, data: { error: '网络错误' } };
  }
}

/* ================= 状态 ================= */
// 画质/音质档位：maxBitrate 单位 bps，对应项目订正日志（勘误后）
const QUALITY = {
  video: {
    ld30:  { label: '360P·30',   w: 640,  h: 360,  fps: 30, max: 1500000 },
    sd30:  { label: '480P·30',   w: 854,  h: 480,  fps: 30, max: 2500000 },
    hd60:  { label: '720P·60',   w: 1280, h: 720,  fps: 60, max: 6000000 },
    fhd60: { label: '1080P·60',  w: 1920, h: 1080, fps: 60, max: 15000000 },
    uhd30: { label: '2160P·30',  w: 3840, h: 2160, fps: 30, max: 45000000 },
    uhd60: { label: '2160P·60',  w: 3840, h: 2160, fps: 60, max: 75000000 },
  },
  audio: {
    phone: { label: '电话音质', max: 16000 },
    std:   { label: '普通',     max: 96000 },
    mid:   { label: '较高',     max: 192000 },
    hi:    { label: '极高',     max: 510000 },
  },
};

const state = {
  me: null,
  ws: null,
  cfg: { iceServers: [] },
  keyPair: null, pubB64: '',
  contacts: new Map(),   // vid -> {vid, name, online, pub, key, unread}
  msgs: new Map(),       // vid -> [{self, text, time}]
  selected: null,
  localStream: null, micTrack: null, camTrack: null, shareTrack: null,
  micOn: true, camOn: true, sharing: false,
  quality: { v: 'hd60', a: 'std' },
  call: null,            // {peer, media, role, phase, pc, polite, ...}
  incoming: null,        // {from, media, name, timer}
};

/* ================= 登录 / 注册 ================= */
function showAuth() {
  $('#auth-view').classList.remove('hidden');
  $('#app-view').classList.add('hidden');
}

function switchTab(which) {
  $('#tab-login').classList.toggle('active', which === 'login');
  $('#tab-reg').classList.toggle('active', which === 'reg');
  $('#login-form').classList.toggle('hidden', which !== 'login');
  $('#reg-form').classList.toggle('hidden', which !== 'reg');
  $('#li-err').textContent = '';
  $('#rg-err').textContent = '';
}

async function enterApp(me) {
  state.me = me;
  $('#my-name').textContent = me.name;
  $('#my-vid').textContent = me.vid;
  $('#auth-view').classList.add('hidden');
  $('#app-view').classList.remove('hidden');
  await initKeys();
  connectWS();
}

async function boot() {
  try {
    const cfg = await (await fetch('/api/config')).json();
    state.cfg = cfg;
    if (cfg.regCodeRequired) $('#rg-code-row').classList.remove('hidden');
  } catch (_) { }
  const res = await api('/api/me');
  if (res.ok) enterApp(res.data);
  else showAuth();
}

$('#tab-login').onclick = () => switchTab('login');
$('#tab-reg').onclick = () => switchTab('reg');

$('#login-form').addEventListener('submit', async e => {
  e.preventDefault();
  const res = await api('/api/login', {
    username: $('#li-user').value,
    password: $('#li-pass').value,
  });
  if (res.ok) enterApp(res.data);
  else $('#li-err').textContent = res.data.error || '登录失败';
});

$('#reg-form').addEventListener('submit', async e => {
  e.preventDefault();
  const res = await api('/api/register', {
    username: $('#rg-user').value,
    name: $('#rg-name').value,
    password: $('#rg-pass').value,
    code: $('#rg-code').value,
  });
  if (res.ok) enterApp(res.data);
  else $('#rg-err').textContent = res.data.error || '注册失败';
});

$('#btn-logout').onclick = async () => {
  await api('/api/logout', {});
  location.reload();
};

$('#btn-reconnect').onclick = () => {
  if (state.ws) return;
  reconnectAttempts = 0;
  $('#overlay').classList.add('hidden');
  connectWS();
};

/* ================= 端到端密钥 ================= */
// 每次登录生成临时 ECDH 密钥对，好友之间两两协商对称密钥；
// 私钥不出浏览器，服务器只转发密文，也不留存聊天记录。
async function initKeys() {
  state.keyPair = await crypto.subtle.generateKey(
    { name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits']);
  const raw = await crypto.subtle.exportKey('raw', state.keyPair.publicKey);
  state.pubB64 = b64e(raw);
}

async function deriveKey(c) {
  if (c.key || !c.pub || !state.keyPair) return null;
  try {
    const pub = await crypto.subtle.importKey('raw', b64d(c.pub), { name: 'ECDH', namedCurve: 'P-256' }, false, []);
    const bits = await crypto.subtle.deriveBits({ name: 'ECDH', public: pub }, state.keyPair.privateKey, 256);
    const hk = await crypto.subtle.importKey('raw', bits, 'HKDF', false, ['deriveKey']);
    const ids = [state.me.vid, c.vid].sort().join('|');
    c.key = await crypto.subtle.deriveKey(
      { name: 'HKDF', hash: 'SHA-256', salt: enc.encode('peacefulVA/' + ids), info: enc.encode('chat-e2e-v1') },
      hk, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
    return c.key;
  } catch (_) {
    return null;
  }
}

/* ================= WebSocket 信令 ================= */
function wsSend(o) {
  if (state.ws && state.ws.readyState === 1) state.ws.send(JSON.stringify(o));
}

let reconnectAttempts = 0;

function showOverlay(text, showButton) {
  $('#overlay-text').textContent = text;
  $('#btn-reconnect').classList.toggle('hidden', !showButton);
  $('#overlay').classList.remove('hidden');
}

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/ws`);
  state.ws = ws;
  ws.onopen = () => {
    reconnectAttempts = 0;
    $('#overlay').classList.add('hidden');
    wsSend({ type: 'ready', pub: state.pubB64 });
  };
  ws.onclose = e => {
    state.ws = null;
    if ($('#app-view').classList.contains('hidden')) return;
    endCall(true);
    clearIncoming();
    if (e.code === 1000) return; // 主动退出/刷新
    if (e.code === 4001) {
      showOverlay('该账号已在其他窗口登录', true);
      return;
    }
    if (reconnectAttempts < 5) {
      reconnectAttempts++;
      showOverlay(`连接断开，正在重连（第 ${reconnectAttempts}/5 次）…`, false);
      setTimeout(() => { if (!state.ws) connectWS(); }, Math.min(1000 * 2 ** (reconnectAttempts - 1), 8000));
    } else {
      showOverlay('与服务器失去连接，请检查网络后重连', true);
    }
  };
  ws.onmessage = e => {
    try { handle(JSON.parse(e.data)); } catch (_) { }
  };
}

function handle(m) {
  switch (m.t) {
    case 'contacts': onContacts(m.list); break;
    case 'presence': onPresence(m); break;
    case 'sig': onSig(m.from, m.d); break;
    case 'chat': onChat(m); break;
    case 'call': onIncomingCall(m); break;
    case 'accept': onAccept(m.from); break;
    case 'decline': onCallEnded(m.from, '对方拒绝了通话'); break;
    case 'cancel': onCallEnded(m.from, '对方已取消'); break;
    case 'hangup': onCallEnded(m.from, '对方已挂断'); break;
    case 'contact-removed': onContactRemoved(m.vid); break;
    case 'err': toast(m.msg || '服务器错误'); break;
  }
}

function onContacts(list) {
  const keep = new Set(list.map(x => x.vid));
  for (const vid of [...state.contacts.keys()]) {
    if (!keep.has(vid)) state.contacts.delete(vid);
  }
  for (const it of list) {
    const c = state.contacts.get(it.vid) || { unread: 0 };
    Object.assign(c, { vid: it.vid, name: it.name, online: !!it.online, pub: it.pub || '', key: null });
    state.contacts.set(it.vid, c);
    if (c.online) deriveKey(c);
  }
  renderContacts();
  if (state.selected && !state.contacts.has(state.selected)) {
    selectContact(null);
  } else if (state.selected) {
    updateChatHead();
  }
}

function onPresence(m) {
  const c = state.contacts.get(m.vid);
  if (!c) return;
  c.online = !!m.online;
  if (m.online) {
    c.pub = m.pub || '';
    c.key = null;
    deriveKey(c);
  } else {
    c.pub = '';
    c.key = null;
    if (state.call && state.call.peer === m.vid) endCall(true);
    if (state.incoming && state.incoming.from === m.vid) clearIncoming();
  }
  renderContacts();
  if (state.selected === m.vid) updateChatHead();
}

function onContactRemoved(vid) {
  const c = state.contacts.get(vid);
  if (state.call && state.call.peer === vid) cleanupCall();
  if (state.selected === vid) {
    state.selected = null;
    $('#chat-view').classList.add('hidden');
    $('#empty-hint').classList.remove('hidden');
  }
  state.contacts.delete(vid);
  state.msgs.delete(vid);
  renderContacts();
  if (c) toast(`${c.name} 已将你从好友列表移除`);
}

/* ================= 好友列表 ================= */
function renderContacts() {
  let totalUnread = 0;
  for (const c of state.contacts.values()) totalUnread += c.unread || 0;
  document.title = totalUnread > 0 ? `(${totalUnread}) peacefulVA` : 'peacefulVA';

  const box = $('#contact-list');
  box.innerHTML = '';
  const list = [...state.contacts.values()].sort((a, b) => (b.online - a.online) || a.name.localeCompare(b.name));
  if (list.length === 0) {
    const el = document.createElement('div');
    el.className = 'none';
    el.textContent = '还没有好友，用对方页面上的 ID 互加';
    box.append(el);
    return;
  }
  for (const c of list) {
    const el = document.createElement('div');
    el.className = 'contact' + (state.selected === c.vid ? ' selected' : '');
    const dot = document.createElement('span');
    dot.className = 'dot' + (c.online ? ' on' : '');
    const info = document.createElement('div');
    info.className = 'info';
    const nm = document.createElement('b');
    nm.textContent = c.name;
    const vid = document.createElement('small');
    vid.textContent = c.vid;
    info.append(nm, vid);
    el.append(dot, info);
    if (c.unread > 0) {
      const u = document.createElement('span');
      u.className = 'unread';
      u.textContent = c.unread > 9 ? '9+' : c.unread;
      el.append(u);
    }
    el.onclick = () => selectContact(c.vid);
    box.append(el);
  }
}

$('#add-form').addEventListener('submit', async e => {
  e.preventDefault();
  const input = $('#add-vid');
  const vid = input.value.trim().toLowerCase();
  if (!vid) return;
  const res = await api('/api/contacts', { vid });
  if (!res.ok) {
    toast(res.data.error || '添加失败');
    return;
  }
  input.value = '';
  const c = state.contacts.get(res.data.vid) || { unread: 0 };
  Object.assign(c, { vid: res.data.vid, name: res.data.name, online: !!res.data.online, pub: '', key: null });
  state.contacts.set(c.vid, c);
  renderContacts();
  toast(`已添加 ${c.name}`);
});

/* ================= 聊天 ================= */
function selectContact(vid) {
  if (state.call) {
    toast('通话中，请先挂断');
    return;
  }
  if (state.selected === vid) return;
  state.selected = vid;
  $('#empty-hint').classList.toggle('hidden', !!vid);
  $('#chat-view').classList.toggle('hidden', !vid);
  if (vid) {
    const c = state.contacts.get(vid);
    c.unread = 0;
    $('#chat-input').focus();
  }
  renderContacts();
  renderMsgs();
  updateChatHead();
}

function updateChatHead() {
  const c = state.contacts.get(state.selected);
  if (!c) return;
  $('#peer-name').textContent = c.name;
  const st = $('#peer-status');
  st.textContent = c.online ? '在线' : '不在线';
  st.classList.toggle('on', c.online);
  $('#btn-call-video').disabled = !c.online;
  $('#btn-call-voice').disabled = !c.online;
}

function renderMsgs() {
  const box = $('#msgs');
  box.innerHTML = '';
  const list = state.msgs.get(state.selected) || [];
  for (const m of list) appendMsgEl(m.name, m.text, m.self, m.time);
  box.scrollTop = box.scrollHeight;
}

function appendMsgEl(name, text, self, time) {
  const box = $('#msgs');
  const el = document.createElement('div');
  el.className = 'msg' + (self ? ' self' : '');
  const meta = document.createElement('div');
  meta.className = 'meta';
  meta.textContent = `${name} ${time}`;
  const body = document.createElement('div');
  body.className = 'body';
  body.textContent = text;
  el.append(meta, body);
  box.append(el);
  box.scrollTop = box.scrollHeight;
}

function pushMsg(vid, name, text, self) {
  if (!state.msgs.has(vid)) state.msgs.set(vid, []);
  state.msgs.get(vid).push({ name, text, self, time: nowHM() });
  if (state.selected === vid) {
    appendMsgEl(name, text, self, nowHM());
  } else {
    const c = state.contacts.get(vid);
    if (c && !self) {
      c.unread = (c.unread || 0) + 1;
      renderContacts();
    }
  }
}

async function sendChat() {
  const input = $('#chat-input');
  const text = input.value.trim().slice(0, 2000);
  if (!text || !state.selected) return;
  const c = state.contacts.get(state.selected);
  if (!c || !c.online || !c.key) {
    toast('对方不在线');
    return;
  }
  input.value = '';
  try {
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, c.key, enc.encode(text));
    wsSend({ type: 'chat', to: c.vid, iv: b64e(iv), ct: b64e(ct) });
    pushMsg(c.vid, state.me.name, text, true);
  } catch (_) {
    toast('发送失败');
  }
}

async function onChat(m) {
  const c = state.contacts.get(m.from);
  if (!c || !c.key || !m.iv || !m.ct) return;
  try {
    const plain = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: b64d(m.iv) }, c.key, b64d(m.ct));
    pushMsg(m.from, c.name, dec.decode(plain), false);
  } catch (_) {
    toast('收到一条无法解密的消息');
  }
}

$('#chat-form').addEventListener('submit', e => {
  e.preventDefault();
  sendChat();
});

/* ================= 通话 ================= */
async function ensureMedia(media) {
  if (!navigator.mediaDevices) return;
  try {
    if (media === 'video') {
      state.localStream = await mediaTimeout(navigator.mediaDevices.getUserMedia({
        audio: { echoCancellation: true, noiseSuppression: true },
        video: { width: { ideal: 1280 }, height: { ideal: 720 } },
      }), 8000);
    } else {
      state.localStream = await mediaTimeout(navigator.mediaDevices.getUserMedia({
        audio: { echoCancellation: true, noiseSuppression: true },
      }), 5000);
    }
  } catch (_) {
    try {
      state.localStream = await mediaTimeout(navigator.mediaDevices.getUserMedia({ audio: true }), 5000);
    } catch (_2) {
      state.localStream = null; // 无媒体也能接通（仅文字/信令），画面留空
    }
  }
  if (state.localStream) {
    state.micTrack = state.localStream.getAudioTracks()[0] || null;
    state.camTrack = state.localStream.getVideoTracks()[0] || null;
  }
  state.micOn = !!state.micTrack;
  state.camOn = !!state.camTrack;
  $('#btn-mic').classList.toggle('off', !state.micOn);
  $('#btn-cam').classList.toggle('off', !state.camOn);
  updateControlState();
}

async function startCall(vid, media) {
  const c = state.contacts.get(vid);
  if (!c || !c.online) return toast('对方不在线');
  if (state.call || state.incoming) return toast('当前有通话在进行');
  await ensureMedia(media);
  state.call = { peer: vid, media, role: 'caller', phase: 'ringing', pc: null };
  wsSend({ type: 'call', to: vid, media });
  showCallView();
  $('#call-status').textContent = `正在等待 ${c.name} 接听…`;
  state.call.timer = setTimeout(() => {
    if (state.call && state.call.phase === 'ringing') {
      wsSend({ type: 'cancel', to: vid });
      cleanupCall();
      toast('对方无响应');
    }
  }, 45000);
}

function onIncomingCall(m) {
  if (state.call || state.incoming) {
    wsSend({ type: 'decline', to: m.from });
    return;
  }
  const c = state.contacts.get(m.from);
  const name = c ? c.name : m.from;
  state.incoming = { from: m.from, media: m.media, name };
  $('#incoming-text').textContent = `${name} 请求${m.media === 'video' ? '视频' : '语音'}通话`;
  $('#incoming').classList.remove('hidden');
  state.incoming.timer = setTimeout(() => {
    if (state.incoming && state.incoming.from === m.from) {
      wsSend({ type: 'decline', to: m.from });
      clearIncoming();
    }
  }, 45000);
}

async function acceptIncoming() {
  if (!state.incoming) return;
  const { from, media } = state.incoming;
  clearIncoming();
  await ensureMedia(media);
  state.call = { peer: from, media, role: 'callee', phase: 'connecting', pc: null };
  wsSend({ type: 'accept', to: from });
  showCallView();
  $('#call-status').textContent = '正在接通…';
  createPC(from);
}

function declineIncoming() {
  if (!state.incoming) return;
  const { from } = state.incoming;
  clearIncoming();
  wsSend({ type: 'decline', to: from });
}

function clearIncoming() {
  if (state.incoming && state.incoming.timer) clearTimeout(state.incoming.timer);
  state.incoming = null;
  $('#incoming').classList.add('hidden');
}

function onAccept(from) {
  if (!state.call || state.call.role !== 'caller' || state.call.peer !== from) return;
  clearTimeout(state.call.timer);
  state.call.phase = 'connecting';
  $('#call-status').textContent = '正在接通…';
  createPC(from);
}

function onCallEnded(from, reason) {
  if (state.call && state.call.peer === from) {
    cleanupCall();
    toast(reason);
  }
}

function createPC(vid) {
  const call = state.call;
  if (!call || call.pc) return;
  const pc = new RTCPeerConnection({
    iceServers: state.cfg.iceServers || [],
    bundlePolicy: 'max-bundle',
  });
  call.pc = pc;
  call.polite = state.me.vid > vid;
  call.makingOffer = false;
  call.ignoreOffer = false;
  call.settingRemoteAnswerPending = false;
  call.pendingCands = [];

  if (state.localStream) {
    for (const t of state.localStream.getTracks()) pc.addTrack(t, state.localStream);
  } else {
    pc.createDataChannel('meta');
  }

  pc.onicecandidate = e => { if (e.candidate) wsSend({ type: 'sig', to: vid, data: { cand: e.candidate.toJSON() } }); };
  pc.ontrack = e => {
    const rv = $('#remote-video');
    const stream = e.streams && e.streams[0] ? e.streams[0] : null;
    const hasVideo = !!(stream && stream.getVideoTracks().length > 0);
    if (stream) rv.srcObject = stream;
    if (!hasVideo) rv.srcObject = null;
    $('#voice-avatar').classList.toggle('hidden', hasVideo);
    rv.classList.toggle('hidden', !hasVideo);
  };
  pc.onnegotiationneeded = async () => {
    try {
      call.makingOffer = true;
      await pc.setLocalDescription();
      wsSend({ type: 'sig', to: vid, data: { desc: pc.localDescription } });
    } catch (err) {
      console.error(err);
    } finally {
      call.makingOffer = false;
    }
  };
  pc.onconnectionstatechange = () => {
    if (pc.connectionState === 'connected') {
      call.phase = 'active';
      call.startedAt = Date.now();
      const label = call.media === 'video' ? '视频通话' : '语音通话';
      $('#call-status').textContent = `${label} 00:00`;
      call.tick = setInterval(() => {
        const sec = Math.floor((Date.now() - call.startedAt) / 1000);
        const mm = String(Math.floor(sec / 60)).padStart(2, '0');
        const ss = String(sec % 60).padStart(2, '0');
        $('#call-status').textContent = `${label} ${mm}:${ss}`;
      }, 1000);
      $('#ring-actions').classList.add('hidden');
      $('#active-actions').classList.remove('hidden');
      updateControlState();
      applyQuality();
    } else if (pc.connectionState === 'failed') {
      cleanupCall();
      toast('连接失败');
    }
  };

  const selfV = $('#self-video');
  if (state.localStream) selfV.srcObject = state.localStream;
}

async function onSig(from, d) {
  const call = state.call;
  if (!call || call.peer !== from || !call.pc || !d) return;
  const pc = call.pc;
  try {
    if (d.desc) {
      const readyForOffer = !call.makingOffer && (pc.signalingState === 'stable' || call.settingRemoteAnswerPending);
      const offerCollision = d.desc.type === 'offer' && !readyForOffer;
      call.ignoreOffer = !call.polite && offerCollision;
      if (call.ignoreOffer) return;
      call.settingRemoteAnswerPending = d.desc.type !== 'offer';
      await pc.setRemoteDescription(d.desc);
      call.settingRemoteAnswerPending = false;
      if (d.desc.type === 'offer') {
        await pc.setLocalDescription();
        wsSend({ type: 'sig', to: from, data: { desc: pc.localDescription } });
      }
      const cands = call.pendingCands;
      call.pendingCands = [];
      for (const c of cands) await pc.addIceCandidate(c).catch(() => { });
    } else if (d.cand) {
      if (!pc.remoteDescription) {
        if (call.pendingCands.length < 100) call.pendingCands.push(d.cand);
        return;
      }
      await pc.addIceCandidate(d.cand).catch(err => { if (!call.ignoreOffer) throw err; });
    }
  } catch (err) {
    console.error('signal error', err);
  }
}

function showCallView() {
  $('#chat-view').classList.add('hidden');
  $('#empty-hint').classList.add('hidden');
  $('#call-view').classList.remove('hidden');
  $('#ring-actions').classList.remove('hidden');
  $('#active-actions').classList.add('hidden');
  const call = state.call;
  const voice = !!(call && call.media === 'voice');
  $('#self-video').classList.toggle('hidden', voice);
  $('#remote-video').classList.toggle('hidden', voice);
  $('#voice-avatar').classList.toggle('hidden', !voice);
  if (call) {
    $('#voice-avatar').textContent = (state.contacts.get(call.peer)?.name || '?').charAt(0);
    $('#call-status').textContent = call.role === 'caller'
      ? `正在等待 ${state.contacts.get(call.peer)?.name || '对方'} 接听…`
      : '正在接通…';
  }
}

function updateControlState() {
  $('#btn-mic').disabled = !state.micTrack;
  $('#btn-cam').disabled = !state.camTrack;
}

function endCall(silent) {
  if (!state.call) return;
  if (!silent && state.call.pc) wsSend({ type: 'hangup', to: state.call.peer });
  cleanupCall();
}

function cleanupCall() {
  const call = state.call;
  if (!call) return;
  if (call.timer) clearTimeout(call.timer);
  if (call.tick) clearInterval(call.tick);
  if (call.pc) { try { call.pc.close(); } catch (_) { } }
  if (state.shareTrack) { state.shareTrack.stop(); state.shareTrack = null; }
  state.sharing = false;
  if (state.localStream) state.localStream.getTracks().forEach(t => t.stop());
  state.localStream = null;
  state.micTrack = null;
  state.camTrack = null;
  $('#self-video').srcObject = null;
  $('#remote-video').srcObject = null;
  $('#self-video').classList.remove('hidden');
  $('#remote-video').classList.remove('hidden');
  $('#voice-avatar').classList.add('hidden');
  $('#btn-mic').classList.remove('off');
  $('#btn-cam').classList.remove('off');
  $('#btn-share').classList.remove('active');
  $('#btn-mic').disabled = true;
  $('#btn-cam').disabled = true;
  $('#call-view').classList.add('hidden');
  state.call = null;
  if (state.selected) {
    $('#chat-view').classList.remove('hidden');
  } else {
    $('#empty-hint').classList.remove('hidden');
  }
}

/* 通话内控制 */
// 画质/音质实时切换：applyConstraints 改采集分辨率不需要重协商，
// 编码上限走 sender.setParameters，对端自动跟随
async function applyQuality() {
  if (!state.call || !state.call.pc) return;
  const q = QUALITY.video[state.quality.v];
  const aq = QUALITY.audio[state.quality.a];
  const vt = state.camTrack || state.shareTrack;
  if (vt && q) {
    try {
      await vt.applyConstraints({ width: q.w, height: q.h, frameRate: q.fps });
    } catch (_) {
      toast('当前设备不支持该分辨率，已保持原画质');
    }
  }
  for (const [kind, target] of [['video', q], ['audio', aq]]) {
    if (!target) continue;
    const sender = state.call.pc.getSenders().find(s => s.track && s.track.kind === kind);
    if (!sender) continue;
    try {
      const p = sender.getParameters();
      if (!p.encodings || p.encodings.length === 0) p.encodings = [{}];
      p.encodings[0].maxBitrate = target.max;
      await sender.setParameters(p);
    } catch (_) { }
  }
}

function initQualityBar() {
  const sv = $('#sel-vq'), sa = $('#sel-aq');
  for (const [k, q] of Object.entries(QUALITY.video)) {
    const o = document.createElement('option');
    o.value = k; o.textContent = q.label;
    sv.append(o);
  }
  for (const [k, q] of Object.entries(QUALITY.audio)) {
    const o = document.createElement('option');
    o.value = k; o.textContent = `${q.label}·${Math.round(q.max / 1000)}k`;
    sa.append(o);
  }
  sv.value = state.quality.v;
  sa.value = state.quality.a;
  sv.onchange = () => { state.quality.v = sv.value; applyQuality(); };
  sa.onchange = () => { state.quality.a = sa.value; applyQuality(); };
}

$('#btn-mic').onclick = () => {
  if (!state.micTrack) return;
  state.micOn = !state.micOn;
  state.micTrack.enabled = state.micOn;
  $('#btn-mic').classList.toggle('off', !state.micOn);
};
$('#btn-cam').onclick = () => {
  if (!state.camTrack) return;
  state.camOn = !state.camOn;
  state.camTrack.enabled = state.camOn;
  $('#btn-cam').classList.toggle('off', !state.camOn);
};
$('#btn-share').onclick = async () => {
  if (!state.call) return;
  if (state.sharing) return stopShare();
  if (!navigator.mediaDevices || !navigator.mediaDevices.getDisplayMedia) return toast('当前环境不支持屏幕共享');
  try {
    const ds = await navigator.mediaDevices.getDisplayMedia({ video: true });
    const track = ds.getVideoTracks()[0];
    track.addEventListener('ended', stopShare);
    state.shareTrack = track;
    state.sharing = true;
    $('#btn-share').classList.add('active');
    await replaceVideoForAll(track);
    $('#self-video').srcObject = ds;
  } catch (_) {
    toast('屏幕共享已取消');
  }
};
async function stopShare() {
  if (!state.sharing) return;
  state.sharing = false;
  if (state.shareTrack) state.shareTrack.stop();
  state.shareTrack = null;
  $('#btn-share').classList.remove('active');
  await replaceVideoForAll(state.camTrack);
  $('#self-video').srcObject = state.localStream;
}

async function replaceVideoForAll(track) {
  if (!state.call || !state.call.pc) return;
  const pc = state.call.pc;
  const jobs = [];
  const sender = pc.getSenders().find(s => s.track && s.track.kind === 'video');
  if (sender) {
    if (track) {
      jobs.push(sender.replaceTrack(track).catch(() => { }));
    } else {
      try { pc.removeTrack(sender); } catch (_) { }
    }
  } else if (track) {
    pc.addTrack(track, new MediaStream([track])); // 语音通话中共享屏幕：补一条视频轨并重新协商
  }
  await Promise.allSettled(jobs);
}

/* ================= 绑定按钮 ================= */
$('#btn-call-video').onclick = () => state.selected && startCall(state.selected, 'video');
$('#btn-call-voice').onclick = () => state.selected && startCall(state.selected, 'voice');
$('#btn-accept').onclick = acceptIncoming;
$('#btn-decline').onclick = declineIncoming;
$('#btn-cancel-call').onclick = () => {
  if (state.call && state.call.role === 'caller') {
    wsSend({ type: 'cancel', to: state.call.peer });
    cleanupCall();
  }
};
$('#btn-hangup').onclick = () => endCall(false);
$('#btn-del-contact').onclick = async () => {
  const vid = state.selected;
  if (!vid) return;
  const c = state.contacts.get(vid);
  if (!c) return;
  if (!confirm(`删除好友「${c.name}」？双方的好友关系都会解除，聊天记录清空`)) return;
  const res = await api('/api/contacts/' + vid, null, 'DELETE');
  if (!res.ok) {
    toast(res.data.error || '删除失败');
    return;
  }
  if (state.call && state.call.peer === vid) cleanupCall();
  state.selected = null;
  $('#chat-view').classList.add('hidden');
  $('#empty-hint').classList.remove('hidden');
  state.contacts.delete(vid);
  state.msgs.delete(vid);
  renderContacts();
  toast('已删除');
};
$('#btn-copy-vid').onclick = async () => {
  try {
    await navigator.clipboard.writeText(state.me.vid);
    toast('ID 已复制，发给对方添加即可');
  } catch (_) {
    toast('复制失败：' + state.me.vid, 6000);
  }
};

/* ================= 启动 ================= */
window.addEventListener('load', () => {
  initQualityBar();
  boot();
});
