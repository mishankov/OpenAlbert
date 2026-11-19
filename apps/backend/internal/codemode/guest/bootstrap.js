(() => {
  const enqueueTool = globalThis.__openalbertEnqueue;
  delete globalThis.__openalbertEnqueue;
  const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
  const parse = JSON.parse;
  const stringify = JSON.stringify;
  const own = Object.hasOwn;
  const outputLimit = 32 * 1024;
  const stateLimit = 16 * 1024;
  const maxBlocks = 128;
  const pending = new Map();
  let state;
  let status = "running";
  let error = "";
  const blocks = [];
  let outputSize = 0;

  function text(value) {
    const encoded = stringify(value) ?? "null";
    if (blocks.length === maxBlocks || encoded.length + outputSize + 2 > outputLimit) {
      throw new RangeError("Codemode output limit exceeded");
    }
    outputSize += encoded.length + 1;
    blocks.push(encoded);
  }

  function fail(reason) {
    status = "failed";
    try {
      error = String(reason?.message ?? reason).slice(0, 1000) || "Codemode script failed";
    } catch {
      error = "Codemode script failed";
    }
  }

  function start(rawInput) {
    const { code, tools: catalog, state: initialState } = parse(rawInput);
    state = Object.assign(Object.create(null), initialState);
    const tools = Object.create(null);
    const definitions = Object.create(null);
    for (const definition of catalog) {
      const { name } = definition;
      definitions[name] = definition;
      tools[name] = (args = {}) => new Promise((resolve, reject) => {
        const id = enqueueTool(name, stringify(args));
        pending.set(id, { resolve, reject });
      });
    }

    function searchTools(query = "", { limit = 8 } = {}) {
      const terms = String(query).toLowerCase().match(/[a-z0-9]+/g) ?? [];
      const results = catalog.map(({ name, description }) => ({
        name,
        description,
        score: terms.reduce((score, term) =>
          score + (name.includes(term) ? 3 : description.toLowerCase().includes(term) ? 1 : 0), 0),
      }));
      return results.filter(item => terms.length === 0 || item.score > 0)
        .sort((a, b) => b.score - a.score)
        .slice(0, Math.max(0, Math.min(32, limit)))
        .map(({ name, description }) => ({ name, description }));
    }

    function describeTool(name) {
      const definition = definitions[name];
      return definition === undefined ? undefined : parse(stringify(definition));
    }

    function store(key, value) {
      if (typeof key !== "string" || key.length < 1 || key.length > 100) {
        throw new TypeError("Codemode store keys must be strings of 1–100 characters");
      }
      const encoded = stringify(value);
      if (encoded === undefined) {
        throw new TypeError("Codemode can only store JSON values");
      }
      const updated = Object.assign(Object.create(null), state);
      updated[key] = parse(encoded);
      if (stringify(updated).length > stateLimit) {
        throw new RangeError("Codemode store limit exceeded");
      }
      state = updated;
    }

    function load(key) {
      return own(state, key) ? parse(stringify(state[key])) : undefined;
    }

    try {
      Object.defineProperties(globalThis, {
        tools: { value: Object.freeze(tools) },
        searchTools: { value: searchTools },
        describeTool: { value: describeTool },
        store: { value: store },
        load: { value: load },
        text: { value: text },
      });
      const run = new AsyncFunction(`"use strict";\n${code}`);
      run()
        .then(value => {
          if (value !== undefined) {
            text(value);
          }
          status = "completed";
        }).catch(fail);
    } catch (reason) {
      fail(reason);
    }
  }

  function settle(rawReceipts) {
    for (const { id, result, failed } of parse(rawReceipts)) {
      const listener = pending.get(id);
      pending.delete(id);
      if (failed) {
        const error = new Error(result?.error ?? "Tool call failed");
        error.code = result?.error_code;
        error.details = result;
        listener.reject(error);
      } else {
        listener.resolve(result);
      }
    }
  }

  function snapshot() {
    const output = blocks.length <= 1 ? blocks[0] ?? "null" : `[${blocks.join(",")}]`;
    return `{"status":${stringify(status)},"error":${stringify(error)},"output":${output},"state":${stringify(state)}}`;
  }

  return { start, settle, snapshot };
})()
