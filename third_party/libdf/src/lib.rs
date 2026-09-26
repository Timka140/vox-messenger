// nx_df — thin C ABI над upstream `deep_filter` (DeepFilterNet 0.5.6).
//
// Почему не используем upstream feature `capi`:
// libDF/src/capi.rs объявлен как `mod capi` (приватный), поэтому
// re-export через `df::capi` невозможен, а extern-декларации под fat LTO не
// тянут символы в staticlib. Самый надёжный вариант — определить
// #[no_mangle] функции прямо в нашем крейте, форвардя вызовы в `df::tract`.
//
// Код ниже — портированная копия libDF/src/capi.rs (только то, что нам нужно),
// с заменой `crate::tract::*` на `df::tract::*`. Семантика идентична upstream.
//
// Контракт:
//   df_create(path: *const c_char, atten_lim: f32) -> *mut DFState
//     path — путь к .tar.gz архиву модели (НЕ к распакованной папке).
//   df_get_frame_length(st) -> usize  (= 480 для DFN3 @ 48 kHz)
//   df_process_frame(st, input, output) -> f32  (возвращает LSNR)
//     input и output — РАЗНЫЕ буферы длины hop_size.
//   df_set_atten_lim(st, lim_db)
//   df_set_post_filter_beta(st, beta)
//   df_free(st)
//
// Панике upstream (`expect`) приводит к abort процесса — как и в upstream.
// Если нужен softer error reporting, добавить чек указателя до unwrap.

use std::ffi::{c_char, c_float, CStr};
use std::path::PathBuf;

use df::tract::{DfParams, DfTract, RuntimeParams};
use ndarray::{ArrayView2, ArrayViewMut2};

pub struct DFState(DfTract);

impl DFState {
    fn new(model_path: &str, channels: usize, atten_lim: f32) -> Self {
        // Пороги apply_stages. default_with_ch(1) даёт min=-10 / max_erb=30 /
        // max_df=20, что приводит к zero-mask и пропуску DF на границах
        // голоса → заикания, прорывы шума. deep-filter CLI (enhance_wav.rs)
        // использует -15/35/35 — модель всегда обрабатывает обе стадии.
        // Воспроизводим те же дефолты для паритета с CLI.
        let r_params = RuntimeParams::default_with_ch(channels)
            .with_atten_lim(atten_lim)
            .with_thresholds(-15.0, 35.0, 35.0);
        let df_params = DfParams::new(PathBuf::from(model_path))
            .expect("nx_df: could not load model from path");
        let m = DfTract::new(df_params, &r_params)
            .expect("nx_df: could not initialize DeepFilter runtime");
        DFState(m)
    }
}

#[no_mangle]
pub unsafe extern "C" fn df_create(path: *const c_char, atten_lim: c_float) -> *mut DFState {
    if path.is_null() {
        return std::ptr::null_mut();
    }
    let c_str = CStr::from_ptr(path);
    let path = match c_str.to_str() {
        Ok(s) => s,
        Err(_) => return std::ptr::null_mut(),
    };
    let df = DFState::new(path, 1, atten_lim);
    Box::into_raw(Box::new(df))
}

#[no_mangle]
pub unsafe extern "C" fn df_get_frame_length(st: *mut DFState) -> usize {
    let state = st.as_mut().expect("nx_df: invalid pointer");
    state.0.hop_size
}

#[no_mangle]
pub unsafe extern "C" fn df_set_atten_lim(st: *mut DFState, lim_db: c_float) {
    let state = st.as_mut().expect("nx_df: invalid pointer");
    state.0.set_atten_lim(lim_db)
}

#[no_mangle]
pub unsafe extern "C" fn df_set_post_filter_beta(st: *mut DFState, beta: c_float) {
    let state = st.as_mut().expect("nx_df: invalid pointer");
    state.0.set_pf_beta(beta)
}

#[no_mangle]
pub unsafe extern "C" fn df_process_frame(
    st: *mut DFState,
    input: *mut c_float,
    output: *mut c_float,
) -> c_float {
    let state = st.as_mut().expect("nx_df: invalid pointer");
    let hop = state.0.hop_size;
    let input = ArrayView2::from_shape_ptr((1, hop), input);
    let output = ArrayViewMut2::from_shape_ptr((1, hop), output);
    state
        .0
        .process(input, output)
        .expect("nx_df: df_process_frame failed")
}

#[no_mangle]
pub unsafe extern "C" fn df_free(st: *mut DFState) {
    if !st.is_null() {
        drop(Box::from_raw(st));
    }
}
