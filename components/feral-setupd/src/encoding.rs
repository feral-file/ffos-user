use crate::constant;

pub fn decode_varint(buf: &[u8]) -> Option<(u64, usize)> {
    let mut value = 0u64;
    let mut shift = 0;
    for (i, &byte) in buf.iter().enumerate() {
        value |= ((byte & 0x7F) as u64) << shift;
        if byte & 0x80 == 0 {
            return Some((value, i + 1));
        }
        shift += 7;
    }
    None
}

/// Parse the payload into a list of values:
pub fn parse_payload(buf: &[u8]) -> Option<Vec<String>> {
    let mut vals = Vec::new();
    let mut cursor = 0;

    while cursor < buf.len() {
        let (val_len, varint_len) = decode_varint(&buf[cursor..])?;
        let val_start = cursor + varint_len;
        let val_end = val_start + val_len as usize;
        let val = std::str::from_utf8(&buf[val_start..val_end])
            .ok()?
            .to_string();
        vals.push(val);
        cursor = val_end;
    }
    Some(vals)
}

pub fn encode_varint(mut value: u64) -> Vec<u8> {
    // the length of value in this app is usually small
    // so we don't want to pre-allocate a large buffer
    let mut buf = Vec::new();
    while value >= 0x80 {
        buf.push((value as u8) | 0x80);
        value >>= 7;
    }
    buf.push(value as u8);
    buf
}

pub fn encode_payload(vals: &[&[u8]]) -> Vec<u8> {
    let mut buf = Vec::new();
    for val in vals {
        buf.extend_from_slice(&encode_varint(val.len() as u64));
        buf.extend_from_slice(val);
    }
    buf
}

pub fn get_device_id() -> String {
    let mac_address = mac_address::get_mac_address().unwrap_or(None);
    let mac_address = match mac_address {
        Some(mac) => mac.bytes(),
        None => [0_u8; 6],
    };

    let digest = md5::compute(mac_address);
    let mut s = String::with_capacity(constant::MD5_LENGTH);
    for &b in &digest[..constant::MD5_LENGTH] {
        let v = b % 36;
        let c = if v < 10 {
            (v + 48) as char // '0'..'9'
        } else {
            (v + 55) as char // 'A'..'Z'
        };
        s.push(c);
    }
    format!("{}{}", constant::DEVICE_ID_PREFIX, s)
}
