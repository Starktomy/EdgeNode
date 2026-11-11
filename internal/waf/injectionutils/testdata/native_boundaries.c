/* Ordinary boundary regressions for the vendored libinjection functions. */
#include "../libinjection_xss.c"

#include <assert.h>
#include <stdlib.h>

void run_sql_boundary_checks(void);

int main(void) {
    const char short_entity[] = {'&', '#'};
    const char short_hex[] = {'&', '#', 'x'};
    const char partial_attribute[] = {'o', 'n', 'a', 'n', 'i'};
    const char ordinary_html[] = "<p \xff data-id=ok>";
    h5_state_t state;
    size_t consumed = 9;
    int result;
    int found_attribute = 0;

    assert(html_decode_char_at(NULL, 0, &consumed) == -1 && consumed == 0);
    assert(html_decode_char_at(short_entity, sizeof(short_entity), &consumed) == '&' && consumed == 1);
    assert(html_decode_char_at(short_hex, sizeof(short_hex), &consumed) == '&' && consumed == 1);
    assert(html_decode_char_at("&#65;", 5, &consumed) == 'A' && consumed == 5);
    assert(html_decode_char_at("&#x41;", 6, &consumed) == 'A' && consumed == 6);
    assert(is_black_attr(partial_attribute, sizeof(partial_attribute), 0) == TYPE_NONE);

    memset(&state, 0, sizeof(state));
    assert(libinjection_h5_next(NULL) == LIBINJECTION_RESULT_ERROR);
    assert(libinjection_h5_next(&state) == LIBINJECTION_RESULT_ERROR);
    libinjection_h5_init(&state, "x", 1, DATA_STATE);
    state.pos = 2;
    assert(libinjection_h5_next(&state) == LIBINJECTION_RESULT_ERROR);
    assert(libinjection_xss(NULL, 1, 0) == LIBINJECTION_RESULT_ERROR);
    assert(libinjection_xss(NULL, 0, 0) == LIBINJECTION_RESULT_FALSE);

    libinjection_h5_init(&state, ordinary_html, sizeof(ordinary_html) - 1, DATA_STATE);
    while ((result = libinjection_h5_next(&state)) == LIBINJECTION_RESULT_TRUE) {
        if (state.token_type == ATTR_NAME && state.token_len == 7 &&
            memcmp(state.token_start, "data-id", 7) == 0) {
            found_attribute = 1;
        }
    }
    assert(result == LIBINJECTION_RESULT_FALSE && found_attribute);

    run_sql_boundary_checks();
    puts("Native boundary checks passed.");
    return 0;
}
