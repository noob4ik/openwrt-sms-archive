'use strict';
'require view';
'require rpc';
'require poll';
'require ui';

const listMessages = rpc.declare({
	object: 'luci.sms-archive',
	method: 'list',
	reject: true,
	params: [ 'page', 'sender', 'sim_id' ]
});
const getStatus = rpc.declare({ object: 'luci.sms-archive', method: 'status', reject: true });
const renameSIM = rpc.declare({
	object: 'luci.sms-archive',
	method: 'rename',
	reject: true,
	params: [ 'sim_id', 'name' ]
});

const dash = '—';
const stateNames = {
	starting: 'Запуск',
	importing: 'Первичный импорт',
	ready: 'Работает',
	error: 'Ошибка',
	disconnected: 'Модем отключён'
};
const saveErrors = {
	invalid_name: 'Имя должно содержать не более 64 символов без управляющих знаков.',
	unknown_sim: 'Карта не найдена среди обнаруженных SIM.',
	save_failed: 'Не удалось сохранить имя SIM.'
};

function plain(value) {
	return document.createTextNode(String(value ?? dash));
}

function simName(id, label) {
	return id ? (label || ('SIM ' + id.slice(-8))) : 'SIM неизвестна';
}

function dateLabel(value, timezone) {
	if (!value)
		return dash;
	const date = new Date(value);
	if (Number.isNaN(date.getTime()))
		return dash;
	try {
		return date.toLocaleString(undefined, { timeZone: timezone || 'UTC' });
	} catch (error) {
		return date.toISOString();
	}
}

function button(title, handler) {
	return E('button', { type: 'button', class: 'btn cbi-button', click: handler }, plain(title));
}

function option(value, title) {
	return E('option', { value }, plain(title));
}

function section(title, contents) {
	return E('section', { class: 'cbi-section' }, [
		E('h3', {}, plain(title)), contents
	]);
}

function statusRows(status) {
	if (!status)
		return [ [ 'Служба', 'Не удалось получить состояние службы.' ] ];
	const state = status.running === false ? 'Остановлена' : (stateNames[status.state] || dash);
	const stale = status.running === false || status.state === 'disconnected' ||
		status.state === 'error';
	const usage = (used, total) => total == null || used == null ? dash :
		used + ' из ' + total + (stale ? ' (последние данные)' : '');
	return [
		[ 'Служба', state ],
		[ 'Активная SIM', !status.active_sim || stale ? dash : simName(status.active_sim, status.active_sim_label) ],
		[ 'Последняя попытка', dateLabel(status.last_attempt, status.timezone) ],
		[ 'Последний успешный цикл', dateLabel(status.last_success, status.timezone) ],
		[ 'Занято на SIM', usage(status.sm_used, status.sm_total) ],
		[ 'Занято в памяти модема', usage(status.me_used, status.me_total) ],
		[ 'Целевой лимит SIM', status.keep_slots ],
		[ 'Сообщений в архиве', status.archived ],
		[ 'Импортировано за цикл', status.imported ],
		[ 'Освобождено ячеек за цикл', status.removed ],
		[ 'Последняя ошибка', status.error || dash ]
	];
}

function messagesTable(result) {
	if (result.error)
		return E('p', {}, plain('Не удалось применить фильтр.'));
	if (!result.messages.length)
		return E('p', {}, plain('Сообщения не найдены.'));

	const labels = new Map(result.sims.map(sim => [sim.id, sim.label]));
	const table = E('table', { class: 'table' }, [
		E('tr', { class: 'tr table-titles' },
			[ 'Время', 'Отправитель', 'SIM-карта', 'Хранилище', 'Сообщение' ]
				.map(title => E('th', { class: 'th' }, plain(title))))
	]);
	for (const message of result.messages) {
		const actualDate = message.received_at || message.archived_at;
		const date = dateLabel(message.display_at || actualDate, result.timezone) +
			(message.received_at ? '' : ' (время архивации)');
		const sources = (message.sources || []).map(source => ({
			SM: 'SIM', ME: 'Модем'
		})[source] || source).join(', ');
		const text = message.text || '';
		const preview = text.split(/\r?\n/)[0];
		const incomplete = message.parts_received < message.parts_expected;
		const summary = (preview.length > 80 ? preview.slice(0, 80) + '…' : preview) ||
			'Открыть сообщение';
		const details = E('details', {}, [
			E('summary', {}, plain(summary + (incomplete ?
				' (' + message.parts_received + ' из ' + message.parts_expected + ' частей)' : ''))),
			E('p', { style: 'white-space: pre-wrap; overflow-wrap: anywhere' }, plain(text || dash))
		]);
		const cells = [ date, message.sender || dash, simName(message.sim_id, labels.get(message.sim_id)), sources || dash ];
		table.appendChild(E('tr', { class: 'tr' }, [
			...cells.map(value => E('td', { class: 'td' }, plain(value))),
			E('td', { class: 'td' }, details)
		]));
	}
	return table;
}

return view.extend({
	handleSave: null,
	handleSaveApply: null,
	handleReset: null,

	render() {
		let page = 1;
		let requestID = 0;
		let saving = false;
		let sims = [];
		const statusBox = E('div', {});
		const history = E('div', {}, plain('Загрузка архива…'));
		const pagination = E('span', { 'aria-live': 'polite' });
		const senderFilter = E('input', { type: 'search', 'aria-label': 'Отправитель', placeholder: 'Отправитель' });
		const simFilter = E('select', { 'aria-label': 'Фильтр SIM' });
		const simSelect = E('select', { 'aria-label': 'SIM для переименования' });
		const simLabel = E('input', { type: 'text', 'aria-label': 'Имя SIM', placeholder: 'Имя SIM, до 64 символов' });
		const prev = button('Назад', () => loadArchive(page - 1));
		const next = button('Дальше', () => loadArchive(page + 1));

		function renderSIMOptions() {
			const filterID = simFilter.value;
			const selectedID = simSelect.value;
			const options = () => sims.map(sim => option(sim.id, simName(sim.id, sim.label)));
			simFilter.replaceChildren(option('', 'Все SIM'), option('unknown', 'SIM неизвестна'), ...options());
			if (filterID === 'unknown' || sims.some(sim => sim.id === filterID))
				simFilter.value = filterID;
			simSelect.replaceChildren(option('', 'Выберите SIM'), ...options());
			if (sims.some(sim => sim.id === selectedID))
				simSelect.value = selectedID;
			renderEditor();
		}

		function renderEditor() {
			const sim = sims.find(sim => sim.id === simSelect.value);
			simLabel.disabled = !sim || saving;
			save.disabled = !sim || saving;
			simLabel.value = sim?.label || '';
		}

		async function loadArchive(targetPage) {
			const id = ++requestID;
			prev.disabled = true;
			next.disabled = true;
			try {
				const result = await listMessages(targetPage, senderFilter.value, simFilter.value);
				if (id !== requestID)
					return;
				if (result.error) {
					history.replaceChildren(plain('Не удалось применить фильтр.'));
					pagination.textContent = '';
					return;
				}
				page = result.page;
				sims = result.sims;
				renderSIMOptions();
				history.replaceChildren(messagesTable(result));
				pagination.textContent = 'Страница ' + page + ' из ' + result.pages +
					' · найдено ' + result.total;
				prev.disabled = page <= 1;
				next.disabled = page >= result.pages;
			} catch (error) {
				if (id === requestID) {
					history.replaceChildren(plain('Не удалось прочитать архив.'));
					pagination.textContent = '';
				}
			}
		}

		async function updateStatus() {
			const status = await getStatus().catch(() => null);
			statusBox.replaceChildren(E('table', { class: 'table' },
				statusRows(status).map(([ title, value ]) => E('tr', { class: 'tr' }, [
					E('th', { class: 'th', scope: 'row' }, plain(title)),
					E('td', { class: 'td' }, plain(value))
				]))));
		}

		const save = button('Сохранить имя', async () => {
			const id = simSelect.value;
			const name = simLabel.value;
			saving = true;
			save.disabled = true;
			simLabel.disabled = true;
			simSelect.disabled = true;
			try {
				const result = await renameSIM(id, name);
				if (!result.ok)
					ui.addNotification(null, E('p', {}, plain(saveErrors[result.error] || 'Не удалось сохранить имя SIM.')), 'error');
				else
					await Promise.all([loadArchive(page), updateStatus()]);
			} catch (error) {
				ui.addNotification(null, E('p', {}, plain('Не удалось сохранить имя SIM.')), 'error');
			} finally {
				saving = false;
				simSelect.disabled = false;
				simLabel.disabled = !simSelect.value;
				save.disabled = !simSelect.value;
			}
		});
		simSelect.addEventListener('change', renderEditor);
		simFilter.addEventListener('change', () => loadArchive(1));
		senderFilter.addEventListener('change', () => loadArchive(1));
		prev.disabled = next.disabled = true;
		renderSIMOptions();
		loadArchive(1);
		updateStatus();
		poll.add(updateStatus, 30);

		return E('div', {}, [
			E('h2', {}, plain('Архив SMS')),
			section('Состояние службы', statusBox),
			section('SIM-карты', E('div', {}, [
				simSelect, ' ', simLabel, ' ', save,
				E('p', {}, plain('Пустое имя возвращает подпись по умолчанию.'))
			])),
			section('История сообщений', E('div', {}, [
				E('div', {}, [
					senderFilter, ' ', simFilter, ' ',
					button('Обновить', () => loadArchive(1))
				]),
				history,
				E('div', {}, [prev, ' ', pagination, ' ', next])
			]))
		]);
	}
});
