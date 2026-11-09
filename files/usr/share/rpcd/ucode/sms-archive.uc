#!/usr/bin/env ucode

'use strict';

import { readfile, stat } from 'fs';
import { cursor } from 'uci';
import { connect } from 'ubus';

const directory = '/var/run/sms-archive/';
const page_size = 50;
const config = 'sms_archive';
const service = connect(null, 2);

function read_snapshot(name, fallback) {
	const path = directory + name;
	const raw = readfile(path);
	if (raw == null) {
		if (stat(path) != null)
			die('Cannot read SMS archive snapshot');
		return fallback;
	}
	const data = json(raw);
	if (type(data) != 'object')
		die('Invalid SMS archive snapshot');
	return data;
}

function archive_index() {
	return read_snapshot('index.json', { messages: [], sims: [], timezone: 'UTC' });
}

function valid_sim_id(id) {
	return type(id) == 'string' && length(id) == 64 && match(id, /^[0-9a-f]+$/);
}

function label_for(uci, id) {
	return valid_sim_id(id) ? (uci.get(config, 's' + id, 'label') || '') : '';
}

function valid_label(label) {
	// Count UTF-8 code points, rejecting C0 and C1 control characters.
	let count = 0;
	for (let i = 0; i < length(label); i++) {
		const byte = ord(label, i);
		if (byte < 32 || byte == 127 ||
		    (byte == 194 && ord(label, i + 1) >= 128 && ord(label, i + 1) <= 159))
			return false;
		if (byte < 128 || byte >= 192)
			count++;
	}
	return count <= 64;
}

function list_messages(request) {
	const args = request.args;
	const sender = lc(trim(args.sender ?? ''));
	const sim_id = args.sim_id ?? '';
	if (length(sender) > 128 || (sim_id != '' && sim_id != 'unknown' && !valid_sim_id(sim_id)))
		return { error: 'invalid_filter' };

	const data = archive_index();
	const uci = cursor();
	const sims = [];
	for (let sim in data.sims)
		if (valid_sim_id(sim.id))
			push(sims, { id: sim.id, label: label_for(uci, sim.id) });

	const matching = [];
	for (let message in data.messages) {
		const source_sim = message.sim_id || 'unknown';
		if (sim_id != '' && source_sim != sim_id)
			continue;
		if (sender != '' && index(lc(message.sender || ''), sender) == -1)
			continue;
		push(matching, message);
	}

	// The daemon writes the read model in descending archive filename order.
	const pages = max(1, int((length(matching) + page_size - 1) / page_size));
	const page = min(pages, max(1, args.page ?? 1));
	const first = (page - 1) * page_size;
	return {
		messages: slice(matching, first, first + page_size),
		sims,
		timezone: data.timezone || 'UTC',
		total: length(matching),
		page,
		pages
	};
}

function service_status(request) {
	const result = read_snapshot('status.json', { state: 'starting' });
	result.active_sim_label = label_for(cursor(), result.active_sim);
	result.timezone = cursor().get_first('system', 'system', 'zonename') || 'UTC';

	if (service == null)
		return result;
	return service.defer('service', 'list', { name: 'sms-archive' }, function(code, reply) {
		if (code == 0) {
			result.running = false;
			for (let instance in values(reply?.['sms-archive']?.instances ?? {}))
				if (instance.running)
					result.running = true;
		}
		request.reply(result);
	});
}

function rename_sim(request) {
	const id = request.args.sim_id;
	const raw = request.args.name;
	if (!valid_sim_id(id) || type(raw) != 'string' || length(raw) > 256)
		return { error: 'invalid_name' };

	// Only remove outside spaces. A control character anywhere is invalid.
	const name = trim(raw, ' ');
	if (!valid_label(name))
		return { error: 'invalid_name' };

	const data = archive_index();
	let known = false;
	for (let sim in data.sims)
		if (sim.id == id)
			known = true;
	if (!known)
		return { error: 'unknown_sim' };

	const uci = cursor();
	const section = 's' + id;
	if (name == '') {
		if (uci.get(config, section, 'label') == null)
			return { ok: true };
		if (!uci.delete(config, section, 'label'))
			return { error: 'save_failed' };
	} else {
		if (uci.get(config, section) == null && !uci.set(config, section, 'sim'))
			return { error: 'save_failed' };
		if (!uci.set(config, section, 'label', name))
			return { error: 'save_failed' };
	}
	return uci.commit(config) ? { ok: true } : { error: 'save_failed' };
}

return {
	'luci.sms-archive': {
		list: { args: { page: 32, sender: '', sim_id: '' }, call: list_messages },
		status: { call: service_status },
		rename: { args: { sim_id: '', name: '' }, call: rename_sim }
	}
};
