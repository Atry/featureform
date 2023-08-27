from behave import *


@then('An exception "{exception}" should be raised')
def step_impl(context, exception):
    if exception == "None":
        assert context.exception is None, f"Exception is {context.exception} not None"
    else:
        assert (
            str(context.exception) == exception
        ), f"\nExpected exception: \n{exception}\nGot: \n{context.exception}"
