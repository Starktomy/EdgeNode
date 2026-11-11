/* SQL numeric tokenization boundaries, in the library's own translation unit. */
#include "../libinjection_sqli.c"

#include <assert.h>

void run_sql_boundary_checks(void) {
    struct libinjection_sqli_state sql;
    libinjection_sqli_init(&sql, "1.e", 3, FLAG_NONE);
    assert(libinjection_sqli_tokenize(&sql) == 0 && sql.pos == 3);
    libinjection_sqli_init(&sql, "12.5", 4, FLAG_NONE);
    assert(libinjection_sqli_tokenize(&sql) == 1 && sql.current->type == TYPE_NUMBER);
}
