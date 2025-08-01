var $jsObjectPtr, $jsErrorPtr;

var $needsExternalization = t => {
    switch (t.kind) {
        case $kindBool:
        case $kindInt:
        case $kindInt8:
        case $kindInt16:
        case $kindInt32:
        case $kindUint:
        case $kindUint8:
        case $kindUint16:
        case $kindUint32:
        case $kindUintptr:
        case $kindFloat32:
        case $kindFloat64:
            return false;
        default:
            return t !== $jsObjectPtr;
    }
};

var $externalize = (v, t, makeWrapper) => {
    if (t === $jsObjectPtr) {
        return v;
    }
    switch (t.kind) {
        case $kindBool:
        case $kindInt:
        case $kindInt8:
        case $kindInt16:
        case $kindInt32:
        case $kindUint:
        case $kindUint8:
        case $kindUint16:
        case $kindUint32:
        case $kindUintptr:
        case $kindFloat32:
        case $kindFloat64:
            return v;
        case $kindInt64:
        case $kindUint64:
            return $flatten64(v);
        case $kindArray:
            if ($needsExternalization(t.elem)) {
                return $mapArray(v, e => { return $externalize(e, t.elem, makeWrapper); });
            }
            return v;
        case $kindFunc:
            return $externalizeFunction(v, t, false, makeWrapper);
        case $kindInterface:
            if (v === $ifaceNil) {
                return null;
            }
            if (v.constructor === $jsObjectPtr) {
                return v.$val.object;
            }
            return $externalize(v.$val, v.constructor, makeWrapper);
        case $kindMap:
            if (v.keys === undefined) {
                return null;
            }
            var m = {};
            var keys = Array.from(v.keys());
            for (var i = 0; i < keys.length; i++) {
                var entry = v.get(keys[i]);
                m[$externalize(entry.k, t.key, makeWrapper)] = $externalize(entry.v, t.elem, makeWrapper);
            }
            return m;
        case $kindPtr:
            if (v === t.nil) {
                return null;
            }
            return $externalize(v.$get(), t.elem, makeWrapper);
        case $kindSlice:
            if (v === v.constructor.nil) {
                return null;
            }
            if ($needsExternalization(t.elem)) {
                return $mapArray($sliceToNativeArray(v), e => { return $externalize(e, t.elem, makeWrapper); });
            }
            return $sliceToNativeArray(v);
        case $kindString:
            if ($isASCII(v)) {
                return v;
            }
            var s = "", r;
            for (var i = 0; i < v.length; i += r[1]) {
                r = $decodeRune(v, i);
                var c = r[0];
                if (c > 0xFFFF) {
                    var h = Math.floor((c - 0x10000) / 0x400) + 0xD800;
                    var l = (c - 0x10000) % 0x400 + 0xDC00;
                    s += String.fromCharCode(h, l);
                    continue;
                }
                s += String.fromCharCode(c);
            }
            return s;
        case $kindStruct:
            var timePkg = $packages["time"];
            if (timePkg !== undefined && v.constructor === timePkg.Time.ptr) {
                var milli = $div64(v.UnixNano(), new $Int64(0, 1000000));
                return new Date($flatten64(milli));
            }

            var noJsObject = {};
            var searchJsObject = (v, t) => {
                if (t === $jsObjectPtr) {
                    return v;
                }
                switch (t.kind) {
                    case $kindPtr:
                        if (v === t.nil) {
                            return noJsObject;
                        }
                        return searchJsObject(v.$get(), t.elem);
                    case $kindStruct:
                        if (t.fields.length === 0) {
                            return noJsObject;
                        }
                        var f = t.fields[0];
                        return searchJsObject(v[f.prop], f.typ);
                    case $kindInterface:
                        return searchJsObject(v.$val, v.constructor);
                    default:
                        return noJsObject;
                }
            };
            var o = searchJsObject(v, t);
            if (o !== noJsObject) {
                return o;
            }

            if (makeWrapper !== undefined) {
                return makeWrapper(v);
            }

            o = {};
            for (var i = 0; i < t.fields.length; i++) {
                var f = t.fields[i];
                if (!f.exported) {
                    continue;
                }
                o[f.name] = $externalize(v[f.prop], f.typ, makeWrapper);
            }
            return o;
    }
    $throwRuntimeError("cannot externalize " + t.string);
};

var $externalizeFunction = (v, t, passThis, makeWrapper) => {
    if (v === $throwNilPointerError) {
        return null;
    }
    if (v.$externalizeWrapper === undefined) {
        $checkForDeadlock = false;
        v.$externalizeWrapper = function () {
            var args = [];
            for (var i = 0; i < t.params.length; i++) {
                if (t.variadic && i === t.params.length - 1) {
                    var vt = t.params[i].elem, varargs = [];
                    for (var j = i; j < arguments.length; j++) {
                        varargs.push($internalize(arguments[j], vt, makeWrapper));
                    }
                    args.push(new (t.params[i])(varargs));
                    break;
                }
                args.push($internalize(arguments[i], t.params[i], makeWrapper));
            }
            var result = v.apply(passThis ? this : undefined, args);
            switch (t.results.length) {
                case 0:
                    return;
                case 1:
                    return $externalize($copyIfRequired(result, t.results[0]), t.results[0], makeWrapper);
                default:
                    for (var i = 0; i < t.results.length; i++) {
                        result[i] = $externalize($copyIfRequired(result[i], t.results[i]), t.results[i], makeWrapper);
                    }
                    return result;
            }
        };
    }
    return v.$externalizeWrapper;
};

var $internalize = (v, t, recv, seen, makeWrapper) => {
    if (t === $jsObjectPtr) {
        return v;
    }
    if (t === $jsObjectPtr.elem) {
        $throwRuntimeError("cannot internalize js.Object, use *js.Object instead");
    }
    if (v && v.__internal_object__ !== undefined) {
        return $assertType(v.__internal_object__, t, false);
    }
    var timePkg = $packages["time"];
    if (timePkg !== undefined && t === timePkg.Time) {
        if (!(v !== null && v !== undefined && v.constructor === Date)) {
            $throwRuntimeError("cannot internalize time.Time from " + typeof v + ", must be Date");
        }
        return timePkg.Unix(new $Int64(0, 0), new $Int64(0, v.getTime() * 1000000));
    }

    // Cache for values we've already internalized in order to deal with circular
    // references.
    if (seen === undefined) { seen = new Map(); }
    if (!seen.has(t)) { seen.set(t, new Map()); }
    if (seen.get(t).has(v)) { return seen.get(t).get(v); }

    switch (t.kind) {
        case $kindBool:
            return !!v;
        case $kindInt:
            return parseInt(v);
        case $kindInt8:
            return parseInt(v) << 24 >> 24;
        case $kindInt16:
            return parseInt(v) << 16 >> 16;
        case $kindInt32:
            return parseInt(v) >> 0;
        case $kindUint:
            return parseInt(v);
        case $kindUint8:
            return parseInt(v) << 24 >>> 24;
        case $kindUint16:
            return parseInt(v) << 16 >>> 16;
        case $kindUint32:
        case $kindUintptr:
            return parseInt(v) >>> 0;
        case $kindInt64:
        case $kindUint64:
            return new t(0, v);
        case $kindFloat32:
        case $kindFloat64:
            return parseFloat(v);
        case $kindArray:
            if (v === null || v === undefined) {
                $throwRuntimeError("cannot internalize "+v+" as a "+t.string);
            }
            if (v.length !== t.len) {
                $throwRuntimeError("got array with wrong size from JavaScript native");
            }
            return $mapArray(v, e => { return $internalize(e, t.elem, makeWrapper); });
        case $kindFunc:
            return function () {
                var args = [];
                for (var i = 0; i < t.params.length; i++) {
                    if (t.variadic && i === t.params.length - 1) {
                        var vt = t.params[i].elem, varargs = arguments[i];
                        for (var j = 0; j < varargs.$length; j++) {
                            args.push($externalize(varargs.$array[varargs.$offset + j], vt, makeWrapper));
                        }
                        break;
                    }
                    args.push($externalize(arguments[i], t.params[i], makeWrapper));
                }
                var result = v.apply(recv, args);
                switch (t.results.length) {
                    case 0:
                        return;
                    case 1:
                        return $internalize(result, t.results[0], makeWrapper);
                    default:
                        for (var i = 0; i < t.results.length; i++) {
                            result[i] = $internalize(result[i], t.results[i], makeWrapper);
                        }
                        return result;
                }
            };
        case $kindInterface:
            if (t.methods.length !== 0) {
                $throwRuntimeError("cannot internalize " + t.string);
            }
            if (v === null) {
                return $ifaceNil;
            }
            if (v === undefined) {
                return new $jsObjectPtr(undefined);
            }
            switch (v.constructor) {
                case Int8Array:
                    return new ($sliceType($Int8))(v);
                case Int16Array:
                    return new ($sliceType($Int16))(v);
                case Int32Array:
                    return new ($sliceType($Int))(v);
                case Uint8Array:
                    return new ($sliceType($Uint8))(v);
                case Uint16Array:
                    return new ($sliceType($Uint16))(v);
                case Uint32Array:
                    return new ($sliceType($Uint))(v);
                case Float32Array:
                    return new ($sliceType($Float32))(v);
                case Float64Array:
                    return new ($sliceType($Float64))(v);
                case Array:
                    return $internalize(v, $sliceType($emptyInterface), makeWrapper);
                case Boolean:
                    return new $Bool(!!v);
                case Date:
                    if (timePkg === undefined) {
                        /* time package is not present, internalize as &js.Object{Date} so it can be externalized into original Date. */
                        return new $jsObjectPtr(v);
                    }
                    return new timePkg.Time($internalize(v, timePkg.Time, makeWrapper));
                case ((() => { })).constructor: // is usually Function, but in Chrome extensions it is something else
                    var funcType = $funcType([$sliceType($emptyInterface)], [$jsObjectPtr], true);
                    return new funcType($internalize(v, funcType, makeWrapper));
                case Number:
                    return new $Float64(parseFloat(v));
                case String:
                    return new $String($internalize(v, $String, makeWrapper));
                default:
                    if ($global.Node && v instanceof $global.Node) {
                        return new $jsObjectPtr(v);
                    }
                    var mapType = $mapType($String, $emptyInterface);
                    return new mapType($internalize(v, mapType, recv, seen, makeWrapper));
            }
        case $kindMap:
            var m = new Map();
            seen.get(t).set(v, m);
            var keys = $keys(v);
            for (var i = 0; i < keys.length; i++) {
                var k = $internalize(keys[i], t.key, recv, seen, makeWrapper);
                m.set(t.key.keyFor(k), { k, v: $internalize(v[keys[i]], t.elem, recv, seen, makeWrapper) });
            }
            return m;
        case $kindPtr:
            if (t.elem.kind === $kindStruct) {
                return $internalize(v, t.elem, makeWrapper);
            }
        case $kindSlice:
            if (v == null) {
                return t.zero();
            }
            return new t($mapArray(v, e => { return $internalize(e, t.elem, makeWrapper); }));
        case $kindString:
            v = String(v);
            if ($isASCII(v)) {
                return v;
            }
            var s = "";
            var i = 0;
            while (i < v.length) {
                var h = v.charCodeAt(i);
                if (0xD800 <= h && h <= 0xDBFF) {
                    var l = v.charCodeAt(i + 1);
                    var c = (h - 0xD800) * 0x400 + l - 0xDC00 + 0x10000;
                    s += $encodeRune(c);
                    i += 2;
                    continue;
                }
                s += $encodeRune(h);
                i++;
            }
            return s;
        case $kindStruct:
            var noJsObject = {};
            var searchJsObject = t => {
                if (t === $jsObjectPtr) {
                    return v;
                }
                if (t === $jsObjectPtr.elem) {
                    $throwRuntimeError("cannot internalize js.Object, use *js.Object instead");
                }
                switch (t.kind) {
                    case $kindPtr:
                        return searchJsObject(t.elem);
                    case $kindStruct:
                        if (t.fields.length === 0) {
                            return noJsObject;
                        }
                        var f = t.fields[0];
                        var o = searchJsObject(f.typ);
                        if (o !== noJsObject) {
                            var n = new t.ptr();
                            n[f.prop] = o;
                            return n;
                        }
                        return noJsObject;
                    default:
                        return noJsObject;
                }
            };
            var o = searchJsObject(t);
            if (o !== noJsObject) {
                return o;
            }
            var n = new t.ptr();
            for (var i = 0; i < t.fields.length; i++) {
                var f = t.fields[i];
                if (!f.exported) {
                    continue;
                }
                var jsProp = v[f.name];
                n[f.prop] = $internalize(jsProp, f.typ, recv, seen, makeWrapper);
            }
            return n;
    }
    $throwRuntimeError("cannot internalize " + t.string);
};

var $copyIfRequired = (v, typ) => {
    // interface values
    if (v && v.constructor && v.constructor.copy) {
        return new v.constructor($clone(v.$val, v.constructor))
    }
    // array and struct values
    if (typ.copy) {
        var clone = typ.zero();
        typ.copy(clone, v);
        return clone;
    }
    return v;
}

/* $isASCII reports whether string s contains only ASCII characters. */
var $isASCII = s => {
    for (var i = 0; i < s.length; i++) {
        if (s.charCodeAt(i) >= 128) {
            return false;
        }
    }
    return true;
};
