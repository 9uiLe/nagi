import Testing
@testable import NagiFixture

@Test func greetingContainsName() {
    #expect(Greeter.message(for: "Nagi") == "Hello, Nagi!")
}
